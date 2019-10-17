package module_manager

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/flant/addon-operator/pkg/app"
	log "github.com/sirupsen/logrus"

	hook_config "github.com/flant/shell-operator/pkg/hook"

	"github.com/flant/addon-operator/pkg/helm"
	"github.com/flant/addon-operator/pkg/kube_config_manager"
	"github.com/flant/addon-operator/pkg/utils"
)

// TODO separate modules and hooks storage, values storage and actions

type ModuleManager interface {
	Init() error
	Run()
	DiscoverModulesState(logLabels map[string]string) (*ModulesState, error)
	GetModule(name string) (*Module, error)
	GetModuleNamesInOrder() []string
	GetGlobalHook(name string) (*GlobalHook, error)
	GetModuleHook(name string) (*ModuleHook, error)
	GetGlobalHooksInOrder(bindingType BindingType) []string
	GetModuleHooksInOrder(moduleName string, bindingType BindingType) ([]string, error)
	DeleteModule(moduleName string, logLabels map[string]string) error
	RunModule(moduleName string, onStartup bool, logLabels map[string]string) error
	RunGlobalHook(hookName string, binding BindingType, bindingContext []BindingContext, logLabels map[string]string) error
	RunModuleHook(hookName string, binding BindingType, bindingContext []BindingContext, logLabels map[string]string) error
	RegisterModuleHooks(module *Module, logLabels map[string]string) error
	Retry()
	WithDirectories(modulesDir string, globalHooksDir string, tempDir string) ModuleManager
	WithKubeConfigManager(kubeConfigManager kube_config_manager.KubeConfigManager) ModuleManager
}

// ModulesState is a result of Discovery process, that determines which
// modules should be enabled, disabled or purged.
type ModulesState struct {
	// modules that should be run
	EnabledModules         []string
	// modules that should be deleted
	ModulesToDisable       []string
	// modules that should be purged
	ReleasedUnknownModules []string
	// modules that was disabled and now are enabled
	NewlyEnabledModules    []string
}

type MainModuleManager struct {
	// Directories
	ModulesDir     string
	GlobalHooksDir string
	TempDir        string

	// Index of all modules in modules directory. Key is module name.
	allModulesByName map[string]*Module

	// Ordered list of all modules names for ordered iterations of allModulesByName.
	allModulesNamesInOrder []string

	// List of modules enabled by values.yaml or by kube config.
	// This list is changed on ConfigMap updates.
	enabledModulesByConfig []string

	// Effective list of enabled modules after enabled script running.
	// List is sorted by module name.
	// This list is changed on ConfigMap changes.
	enabledModulesInOrder []string

	// Index of all global hooks. Key is global hook name
	globalHooksByName map[string]*GlobalHook
	// Index for searching global hooks by their bindings.
	globalHooksOrder map[BindingType][]*GlobalHook

	// module hooks by module name and binding type ordered by name
	// Note: one module hook can have several binding types.
	modulesHooksOrderByName map[string]map[BindingType][]*ModuleHook

	// all values from modules/values.yaml file
	commonStaticValues utils.Values
	// global section from modules/values.yaml file
	globalCommonStaticValues utils.Values

	// global values from ConfigMap
	kubeGlobalConfigValues utils.Values
	// module values from ConfigMap, only for enabled modules
	kubeModulesConfigValues map[string]utils.Values

	// Invariant: do not store patches that cannot be applied.
	// Give user error for patches early, after patch receive.

	// Patches for dynamic global values
	globalDynamicValuesPatches []utils.ValuesPatch
	// Pathces for dynamic module values
	modulesDynamicValuesPatches map[string][]utils.ValuesPatch

	// Internal event: module values are changed.
	// This event leads to module run action.
	moduleValuesChanged chan string
	// Internal event: global values are changed.
	// This event leads to module discovery action.
	globalValuesChanged chan bool

	helm              helm.HelmClient
	kubeConfigManager kube_config_manager.KubeConfigManager

	// Saved values from ConfigMap to handle Ambigous state.
	moduleConfigsUpdateBeforeAmbiguos kube_config_manager.ModuleConfigs
	// Internal event: module manager needs to be restarted.
	retryOnAmbigous chan bool
}

var _ ModuleManager = &MainModuleManager{}

var (
	EventCh chan Event
)

// BindingType is types of events that can trigger hooks.
type BindingType string

const (
	BeforeHelm      BindingType = "BEFORE_HELM"
	AfterHelm       BindingType = "AFTER_HELM"
	AfterDeleteHelm BindingType = "AFTER_DELETE_HELM"
	BeforeAll       BindingType = "BEFORE_ALL"
	AfterAll        BindingType = "AFTER_ALL"
	Schedule        BindingType = "SCHEDULE"
	OnStartup       BindingType = "ON_STARTUP"
	KubeEvents      BindingType = "KUBE_EVENTS"
)

// ContextBindingType is a reverse index for BindingType constants to use in BINDING_CONTEXT_PATH file.
var ContextBindingType = map[BindingType]string{
	BeforeHelm:      "beforeHelm",
	AfterHelm:       "afterHelm",
	AfterDeleteHelm: "afterDeleteHelm",
	BeforeAll:       "beforeAll",
	AfterAll:        "afterAll",
	Schedule:        "schedule",
	OnStartup:       "onStartup",
	KubeEvents:      "onKubernetesEvent",
}

var ShOpBindingType = map[BindingType]hook_config.BindingType{
	OnStartup:       hook_config.OnStartup,
	Schedule:        hook_config.Schedule,
	KubeEvents:      hook_config.OnKubernetesEvent,
}

// BindingContext is a json with additional info for schedule and onKubeEvent hooks
type BindingContext struct {
	hook_config.BindingContext
}

// EventType are events for the main loop.
type EventType string

const (
	// There are modules with changed values.
	ModulesChanged EventType = "MODULES_CHANGED"
	// Global section is changed.
	GlobalChanged EventType = "GLOBAL_CHANGED"
	// Something wrong with module manager.
	AmbigousState EventType = "AMBIGOUS_STATE"
)

// ChangeType are types of module changes.
type ChangeType string

const (
	// All other types are deprecated. This const can be removed in future versions.
	// Module values are changed
	Changed ChangeType = "MODULE_CHANGED"
)

// ModuleChange contains module name and type of module changes.
type ModuleChange struct {
	Name       string
	ChangeType ChangeType
}

// Event is used to send module events to the main loop.
type Event struct {
	ModulesChanges []ModuleChange
	Type           EventType
}

// Init loads global hooks configs, searches for modules, loads values and calculates enabled modules
func Init() {
	log.WithField("operator.phase", "init").Debug("INIT: module_manager")

	EventCh = make(chan Event, 1)
	return
}

// NewMainModuleManager returns new MainModuleManager
func NewMainModuleManager() *MainModuleManager {
	return &MainModuleManager{
		allModulesByName:            make(map[string]*Module),
		allModulesNamesInOrder:      make([]string, 0),
		enabledModulesByConfig:      make([]string, 0),
		enabledModulesInOrder:       make([]string, 0),
		globalHooksByName:           make(map[string]*GlobalHook),
		globalHooksOrder:            make(map[BindingType][]*GlobalHook),
		modulesHooksOrderByName:     make(map[string]map[BindingType][]*ModuleHook),
		commonStaticValues:          make(utils.Values),
		globalCommonStaticValues:    make(utils.Values),
		kubeGlobalConfigValues:      make(utils.Values),
		kubeModulesConfigValues:     make(map[string]utils.Values),
		globalDynamicValuesPatches:  make([]utils.ValuesPatch, 0),
		modulesDynamicValuesPatches: make(map[string][]utils.ValuesPatch),

		moduleValuesChanged: make(chan string, 1),
		globalValuesChanged: make(chan bool, 1),

		kubeConfigManager: nil,

		moduleConfigsUpdateBeforeAmbiguos: make(kube_config_manager.ModuleConfigs),
		retryOnAmbigous:                   make(chan bool, 1),
	}
}

// RunModulesEnabledScript runs enable script for each module that is enabled by config.
// Enable script receives a list of previously enabled modules.
func (mm *MainModuleManager) RunModulesEnabledScript(enabledByConfig []string, logLabels map[string]string) ([]string, error) {
	enabledModules := make([]string, 0)

	for _, name := range utils.SortByReference(enabledByConfig, mm.allModulesNamesInOrder) {
		moduleLogLabels := utils.MergeLabels(logLabels)
		moduleLogLabels["module"] = name
		module := mm.allModulesByName[name]
		moduleIsEnabled, err := module.checkIsEnabledByScript(enabledModules, moduleLogLabels)
		if err != nil {
			return nil, err
		}

		if moduleIsEnabled {
			enabledModules = append(enabledModules, name)
		}
	}

	return enabledModules, nil
}

// kubeUpdate
type kubeUpdate struct {
	EnabledModulesByConfig  []string
	KubeGlobalConfigValues  utils.Values
	KubeModulesConfigValues map[string]utils.Values
	Events                  []Event
}

func (mm *MainModuleManager) applyKubeUpdate(kubeUpdate *kubeUpdate) error {
	log.Debugf("Apply kubeupdate %+v", kubeUpdate)
	mm.kubeGlobalConfigValues = kubeUpdate.KubeGlobalConfigValues
	mm.kubeModulesConfigValues = kubeUpdate.KubeModulesConfigValues
	mm.enabledModulesByConfig = kubeUpdate.EnabledModulesByConfig

	for _, event := range kubeUpdate.Events {
		EventCh <- event
	}

	return nil
}

func (mm *MainModuleManager) handleNewKubeConfig(newConfig kube_config_manager.Config) (*kubeUpdate, error) {
	logEntry := log.WithField("operator.component", "ModuleManager").
		WithField("operator.action", "handleNewKubeConfig")
	logEntry.Debugf("new kube config received")

	res := &kubeUpdate{
		KubeGlobalConfigValues: newConfig.Values,
		Events:                 []Event{{Type: GlobalChanged}},
	}

	var unknown []utils.ModuleConfig
	res.EnabledModulesByConfig, res.KubeModulesConfigValues, unknown = mm.calculateEnabledModulesByConfig(newConfig.ModuleConfigs)

	for _, moduleConfig := range unknown {
		logEntry.Warnf("Ignore kube config for absent module : \n%s",
			moduleConfig.String(),
		)
	}

	return res, nil
}

func (mm *MainModuleManager) handleNewKubeModuleConfigs(moduleConfigs kube_config_manager.ModuleConfigs) (*kubeUpdate, error) {
	logLabels := map[string]string{
		"operator.component": "HandleConfigMap",
	}
	logEntry := log.WithFields(utils.LabelsToLogFields(logLabels))

	logEntry.Debugf("handle changes in module sections")

	res := &kubeUpdate{
		Events:                 make([]Event, 0),
		KubeGlobalConfigValues: mm.kubeGlobalConfigValues,
	}

	// NOTE: values for non changed modules were copied from mm.kubeModulesConfigValues[moduleName].
	// Now calculateEnabledModulesByConfig got values for modules from moduleConfigs — as they are in ConfigMap now.
	// TODO this should not be a problem because of a checksum matching in kube_config_manager
	var unknown []utils.ModuleConfig
	res.EnabledModulesByConfig, res.KubeModulesConfigValues, unknown = mm.calculateEnabledModulesByConfig(moduleConfigs)

	for _, moduleConfig := range unknown {
		logEntry.Warnf("ignore module section for unknown module '%s':\n%s",
			moduleConfig.ModuleName, moduleConfig.String())
	}

	// Detect removed module sections for statically enabled modules.
	// This removal should be handled like kube config update.
	updateAfterRemoval := make(map[string]bool, 0)
	for moduleName, module := range mm.allModulesByName {
		_, hasKubeConfig := moduleConfigs[moduleName]
		if !hasKubeConfig && mergeEnabled(module.CommonStaticConfig.IsEnabled, module.StaticConfig.IsEnabled) {
			if _, hasValues := mm.kubeModulesConfigValues[moduleName]; hasValues {
				updateAfterRemoval[moduleName] = true
			}
		}
	}

	// New version of mm.enabledModulesByConfig
	res.EnabledModulesByConfig = utils.SortByReference(res.EnabledModulesByConfig, mm.allModulesNamesInOrder)

	// Run enable scripts
	logEntry.Debugf("Run enabled script for %+v", res.EnabledModulesByConfig)
	enabledModules, err := mm.RunModulesEnabledScript(res.EnabledModulesByConfig, logLabels)
	if err != nil {
		return nil, err
	}
	logEntry.Infof("Modules enabled by script: %+v", enabledModules)

	// Configure events
	if !reflect.DeepEqual(mm.enabledModulesInOrder, enabledModules) {
		// Enabled modules set is changed — return GlobalChanged event, that will
		// create a Discover task, run enabled scripts again, init new module hooks,
		// update mm.enabledModulesInOrder
		logEntry.Debugf("enabledByConfig changed from %v to %v: generate GlobalChanged event", mm.enabledModulesByConfig, res.EnabledModulesByConfig)
		res.Events = append(res.Events, Event{Type: GlobalChanged})
	} else {
		// Enabled modules set is not changed, only values in configmap are changed.
		logEntry.Debugf("generate ModulesChanged events...")

		moduleChanges := make([]ModuleChange, 0)

		// make Changed event for each enabled module with updated config
		for _, name := range enabledModules {
			// Module has updated kube config
			isUpdated := false
			moduleConfig, hasKubeConfig := moduleConfigs[name]

			if hasKubeConfig {
				isUpdated = moduleConfig.IsUpdated
				// skip not updated module configs
				if !isUpdated {
					logEntry.Debugf("ignore module '%s': kube config is not updated", name)
					continue
				}
			}

			// Update module if kube config is removed
			_, shouldUpdateAfterRemoval := updateAfterRemoval[name]

			if (hasKubeConfig && isUpdated) || shouldUpdateAfterRemoval {
				moduleChanges = append(moduleChanges, ModuleChange{Name: name, ChangeType: Changed})
			}
		}

		if len(moduleChanges) > 0 {
			logEntry.Infof("fire ModulesChanged event for %d modules", len(moduleChanges))
			logEntry.Debugf("event changes: %v", moduleChanges)
			res.Events = append(res.Events, Event{Type: ModulesChanged, ModulesChanges: moduleChanges})
		}
	}

	return res, nil
}

// calculateEnabledModulesByConfig determine enable state for all modules by values.yaml and ConfigMap configuration.
// Method returns list of enabled modules and their values. Also the map of disabled modules and a list of unknown
// keys in a ConfigMap.
//
// Module is enabled by config if module section in ConfigMap is a map or an array
// or ConfigMap has no module section and module has a map or an array in values.yaml
func (mm *MainModuleManager) calculateEnabledModulesByConfig(moduleConfigs kube_config_manager.ModuleConfigs) (enabled []string, values map[string]utils.Values, unknown []utils.ModuleConfig) {
	values = make(map[string]utils.Values)

	for moduleName, module := range mm.allModulesByName {
		kubeConfig, hasKubeConfig := moduleConfigs[moduleName]
		if hasKubeConfig {
			isEnabled := mergeEnabled(module.CommonStaticConfig.IsEnabled,
			                          module.StaticConfig.IsEnabled,
			                          kubeConfig.IsEnabled)

			if isEnabled {
				enabled = append(enabled, moduleName)
				values[moduleName] = kubeConfig.Values
			}
			log.Debugf("Module %s: static enabled %v, kubeConfig: enabled %v, updated %v",
				module.Name,
				module.StaticConfig.IsEnabled,
				kubeConfig.IsEnabled,
				kubeConfig.IsUpdated)
		} else {
			isEnabled := mergeEnabled(module.CommonStaticConfig.IsEnabled, module.StaticConfig.IsEnabled)
			if isEnabled {
				enabled = append(enabled, moduleName)
			}
			log.Debugf("Module %s: static enabled %v, no kubeConfig", module.Name, module.StaticConfig.IsEnabled)
		}
	}

	for _, kubeConfig := range moduleConfigs {
		if _, hasKey := mm.allModulesByName[kubeConfig.ModuleName]; !hasKey {
			unknown = append(unknown, kubeConfig)
		}
	}

	enabled = utils.SortByReference(enabled, mm.allModulesNamesInOrder)

	return
}

// Init — initialize module manager
func (mm *MainModuleManager) Init() error {
	log.Debug("Init ModuleManager")

	if err := mm.RegisterGlobalHooks(); err != nil {
		return err
	}

	if err := mm.RegisterModules(); err != nil {
		return err
	}

	kubeConfig := mm.kubeConfigManager.InitialConfig()
	mm.kubeGlobalConfigValues = kubeConfig.Values

	var unknown []utils.ModuleConfig
	mm.enabledModulesByConfig, mm.kubeModulesConfigValues, unknown = mm.calculateEnabledModulesByConfig(kubeConfig.ModuleConfigs)

	unknownNames := []string{}
	for _, config := range unknown {
		unknownNames = append(unknownNames, config.ModuleName)
	}
	log.Warnf("ConfigMap/%s has values for absent modules: %+v", app.ConfigMapName, unknownNames)

	return nil
}

// Module manager loop
func (mm *MainModuleManager) Run() {
	go mm.kubeConfigManager.Run()

	for {
		select {
		case <-mm.globalValuesChanged:
			log.Debugf("MODULE_MANAGER_RUN global values")
			EventCh <- Event{Type: GlobalChanged}

		case moduleName := <-mm.moduleValuesChanged:
			log.Debugf("MODULE_MANAGER_RUN module '%s' values changed", moduleName)

			// Перезапускать enabled-скрипт не нужно, т.к.
			// изменение values модуля не может вызвать
			// изменение состояния включенности модуля
			EventCh <- Event{
				Type: ModulesChanged,
				ModulesChanges: []ModuleChange{
					{Name: moduleName, ChangeType: Changed},
				},
			}

		case newKubeConfig := <-kube_config_manager.ConfigUpdated:
			handleRes, err := mm.handleNewKubeConfig(newKubeConfig)
			if err != nil {
				log.Errorf("MODULE_MANAGER_RUN unable to handle kube config update: %s", err)
			}
			if handleRes != nil {
				err = mm.applyKubeUpdate(handleRes)
				if err != nil {
					log.Errorf("MODULE_MANAGER_RUN cannot apply kube config update: %s", err)
				}
			}

		case newModuleConfigs := <-kube_config_manager.ModuleConfigsUpdated:
			// Сбросить запомненные перед ошибкой конфиги
			mm.moduleConfigsUpdateBeforeAmbiguos = kube_config_manager.ModuleConfigs{}

			handleRes, err := mm.handleNewKubeModuleConfigs(newModuleConfigs)
			if err != nil {
				mm.moduleConfigsUpdateBeforeAmbiguos = newModuleConfigs
				modulesNames := make([]string, 0)
				for _, newModuleConfig := range newModuleConfigs {
					modulesNames = append(modulesNames, fmt.Sprintf("'%s'", newModuleConfig.ModuleName))
				}
				log.Errorf("MODULE_MANAGER_RUN unable to handle modules %s kube config update: %s", strings.Join(modulesNames, ", "), err)
			}
			if handleRes != nil {
				err = mm.applyKubeUpdate(handleRes)
				if err != nil {
					modulesNames := make([]string, 0)
					for _, newModuleConfig := range newModuleConfigs {
						modulesNames = append(modulesNames, fmt.Sprintf("'%s'", newModuleConfig.ModuleName))
					}
					log.Errorf("MODULE_MANAGER_RUN cannot apply modules %s kube config update: %s", strings.Join(modulesNames, ", "), err)
				}
			}

		case <-mm.retryOnAmbigous:
			if len(mm.moduleConfigsUpdateBeforeAmbiguos) != 0 {
				log.Infof("MODULE_MANAGER_RUN Retry saved moduleConfigs: %v", mm.moduleConfigsUpdateBeforeAmbiguos)
				kube_config_manager.ModuleConfigsUpdated <- mm.moduleConfigsUpdateBeforeAmbiguos
			} else {
				log.Debugf("MODULE_MANAGER_RUN Retry IS NOT needed")
			}
		}
	}
}

func (mm *MainModuleManager) Retry() {
	mm.retryOnAmbigous <- true
}


// DiscoverModulesState handles DiscoverModulesState event: it calculates new arrays of enabled modules,
// modules that should be disabled and modules that should be purged.
//
// This method requires that mm.enabledModulesByConfig and mm.kubeModulesConfigValues are updated.
func (mm *MainModuleManager) DiscoverModulesState(logLabels map[string]string) (state *ModulesState, err error) {
	logEntry := log.WithField("operator.component", "moduleManager,discoverModulesState")

	logEntry.Debugf("DISCOVER state:\n"+
		"    mm.enabledModulesByConfig: %v\n"+
		"    mm.enabledModulesInOrder:  %v\n",
		mm.enabledModulesByConfig,
		mm.enabledModulesInOrder)

	state = &ModulesState{
		EnabledModules: []string{},
		ModulesToDisable: []string{},
		ReleasedUnknownModules: []string{},
		NewlyEnabledModules: []string{},
	}

	releasedModules, err := helm.NewHelmCli(logEntry).ListReleasesNames(nil)
	if err != nil {
		return nil, err
	}

	// calculate unknown released modules to purge them in reverse order
	state.ReleasedUnknownModules = utils.ListSubtract(releasedModules, mm.allModulesNamesInOrder)
	state.ReleasedUnknownModules = utils.SortReverse(state.ReleasedUnknownModules)
	if len(state.ReleasedUnknownModules) > 0 {
		logEntry.Infof("found modules with releases: %s", state.ReleasedUnknownModules)
	}

	// ignore unknown released modules for next operations
	releasedModules = utils.ListIntersection(releasedModules, mm.allModulesNamesInOrder)

	// modules finally enabled with enable script
	// no need to refresh mm.enabledModulesByConfig because
	// it is updated before in Init or in applyKubeUpdate
	logEntry.Debugf("Run enabled script for %+v", mm.enabledModulesByConfig)
	enabledModules, err := mm.RunModulesEnabledScript(mm.enabledModulesByConfig, logLabels)
	logEntry.Infof("Modules enabled by script: %+v", enabledModules)

	if err != nil {
		return nil, err
	}

	for _, moduleName := range enabledModules {
		if err = mm.RegisterModuleHooks(mm.allModulesByName[moduleName], logLabels); err != nil {
			return nil, err
		}
	}

	state.EnabledModules = enabledModules

	state.NewlyEnabledModules = utils.ListSubtract(enabledModules, mm.enabledModulesInOrder)
	// save enabled modules for future usages
	mm.enabledModulesInOrder = enabledModules

	// Calculate modules that has helm release and are disabled for now.
	// Sort them in reverse order for proper deletion.
	state.ModulesToDisable = utils.ListSubtract(mm.allModulesNamesInOrder, enabledModules)
	state.ModulesToDisable = utils.ListIntersection(state.ModulesToDisable, releasedModules)
	state.ModulesToDisable = utils.SortReverseByReference(state.ModulesToDisable, mm.allModulesNamesInOrder)

	logEntry.Debugf("DISCOVER state results:\n"+
		"    mm.enabledModulesByConfig: %v\n"+
		"    EnabledModules: %v\n"+
		"    ReleasedUnknownModules: %v\n"+
		"    ModulesToDisable: %v\n"+
		"    NewlyEnabled: %v\n",
		mm.enabledModulesByConfig,
		mm.enabledModulesInOrder,
		state.ReleasedUnknownModules,
		state.ModulesToDisable,
		state.NewlyEnabledModules)
	return
}

// TODO replace with Module and ModuleShouldExists
func (mm *MainModuleManager) GetModule(name string) (*Module, error) {
	module, exist := mm.allModulesByName[name]
	if exist {
		return module, nil
	} else {
		return nil, fmt.Errorf("module '%s' not found", name)
	}
}

func (mm *MainModuleManager) GetModuleNamesInOrder() []string {
	return mm.enabledModulesInOrder
}

func (mm *MainModuleManager) GetGlobalHook(name string) (*GlobalHook, error) {
	globalHook, exist := mm.globalHooksByName[name]
	if exist {
		return globalHook, nil
	} else {
		return nil, fmt.Errorf("global hook '%s' not found", name)
	}
}

func (mm *MainModuleManager) GetModuleHook(name string) (*ModuleHook, error) {
	for _, bindingHooks := range mm.modulesHooksOrderByName {
		for _, hooks := range bindingHooks {
			for _, hook := range hooks {
				if hook.Name == name {
					return hook, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("module hook '%s' is not found", name)
}

func (mm *MainModuleManager) GetGlobalHooksInOrder(bindingType BindingType) []string {
	globalHooks, ok := mm.globalHooksOrder[bindingType]
	if !ok {
		return []string{}
	}

	sort.Slice(globalHooks[:], func(i, j int) bool {
		return globalHooks[i].Order(bindingType) < globalHooks[j].Order(bindingType)
	})

	var globalHooksNames []string
	for _, globalHook := range globalHooks {
		globalHooksNames = append(globalHooksNames, globalHook.Name)
	}

	return globalHooksNames
}

func (mm *MainModuleManager) GetModuleHooksInOrder(moduleName string, bindingType BindingType) ([]string, error) {
	if _, err := mm.GetModule(moduleName); err != nil {
		return nil, err
	}

	moduleHooksByBinding, ok := mm.modulesHooksOrderByName[moduleName]
	if !ok {
		return []string{}, nil
	}

	moduleBindingHooks, ok := moduleHooksByBinding[bindingType]
	if !ok {
		return []string{}, nil
	}

	sort.Slice(moduleBindingHooks[:], func(i, j int) bool {
		return moduleBindingHooks[i].Order(bindingType) < moduleBindingHooks[j].Order(bindingType)
	})

	var moduleHooksNames []string
	for _, moduleHook := range moduleBindingHooks {
		moduleHooksNames = append(moduleHooksNames, moduleHook.Name)
	}

	return moduleHooksNames, nil
}

// TODO: moduleManager.Module(modName).Delete()
func (mm *MainModuleManager) DeleteModule(moduleName string, logLabels map[string]string) error {
	module, err := mm.GetModule(moduleName)
	if err != nil {
		return err
	}

	if err := module.Delete(logLabels); err != nil {
		return err
	}

	// remove module hooks from indexes
	delete(mm.modulesHooksOrderByName, moduleName)

	return nil
}

// RunModule runs beforeHelm hook, helm upgrade --install and afterHelm or afterDeleteHelm hook
func (mm *MainModuleManager) RunModule(moduleName string, onStartup bool, logLabels map[string]string) error {
	module, err := mm.GetModule(moduleName)
	if err != nil {
		return err
	}

	if err := module.Run(onStartup, logLabels); err != nil {
		return err
	}

	return nil
}

func (mm *MainModuleManager) RunGlobalHook(hookName string, binding BindingType, bindingContext []BindingContext, logLabels map[string]string) error {
	globalHook, err := mm.GetGlobalHook(hookName)
	if err != nil {
		return err
	}

	oldValuesChecksum, err := utils.ValuesChecksum(globalHook.values())
	if err != nil {
		return err
	}

	if err := globalHook.Run(binding, bindingContext, logLabels); err != nil {
		return err
	}

	newValuesChecksum, err := utils.ValuesChecksum(globalHook.values())
	if err != nil {
		return err
	}

	if newValuesChecksum != oldValuesChecksum {
		switch binding {
		case Schedule, KubeEvents:
			mm.globalValuesChanged <- true
		}
	}

	return nil
}

func (mm *MainModuleManager) RunModuleHook(hookName string, binding BindingType, bindingContext []BindingContext, logLabels map[string]string) error {
	moduleHook, err := mm.GetModuleHook(hookName)
	if err != nil {
		return err
	}

	oldValuesChecksum, err := utils.ValuesChecksum(moduleHook.values())
	if err != nil {
		return err
	}

	if err := moduleHook.Run(binding, bindingContext, logLabels); err != nil {
		return err
	}

	newValuesChecksum, err := utils.ValuesChecksum(moduleHook.values())
	if err != nil {
		return err
	}

	if newValuesChecksum != oldValuesChecksum {
		switch binding {
		case Schedule, KubeEvents:
			mm.moduleValuesChanged <- moduleHook.Module.Name
		}
	}

	return nil
}

func (mm *MainModuleManager) WithDirectories(modulesDir string, globalHooksDir string, tempDir string) ModuleManager {
	mm.ModulesDir = modulesDir
	mm.GlobalHooksDir = globalHooksDir
	mm.TempDir = tempDir
	return mm
}

func (mm *MainModuleManager) WithKubeConfigManager(kubeConfigManager kube_config_manager.KubeConfigManager) ModuleManager {
	mm.kubeConfigManager = kubeConfigManager
	return mm
}

// mergeEnabled merges enabled flags. Enabled flag can be nil.
//
// If all flags are nil, then false is returned — module is disabled by default.
//
func mergeEnabled(enabledFlags ... *bool) bool {
	result := false
	for _, enabled := range enabledFlags {
		if enabled == nil {
			continue
		} else {
			result = *enabled
		}
	}

	return result
}
