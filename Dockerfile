# build libjq
FROM alpine:3.12 AS libjq
RUN apk --no-cache add git ca-certificates && \
    git clone https://github.com/flant/libjq-go /libjq-go && \
    cd /libjq-go && \
    git submodule update --init --recursive && \
    /libjq-go/scripts/install-libjq-dependencies-alpine.sh && \
    /libjq-go/scripts/build-libjq-static.sh /libjq-go /libjq


# build addon-operator binary linked with libjq
FROM golang:1.15-alpine3.12 AS addon-operator
ARG appVersion=latest
RUN apk --no-cache add git ca-certificates gcc libc-dev

# Cache-friendly download of go dependencies.
ADD go.mod go.sum /app/
WORKDIR /app
RUN go mod download

COPY --from=libjq /libjq /libjq
ADD . /app
WORKDIR /app

RUN git submodule update --init --recursive && ./go-build.sh $appVersion


# build final image
FROM alpine:3.12
RUN apk --no-cache add ca-certificates jq bash tini && \
    wget https://storage.googleapis.com/kubernetes-release/release/v1.19.4/bin/linux/amd64/kubectl -O /bin/kubectl && \
    chmod +x /bin/kubectl && \
    wget https://get.helm.sh/helm-v3.4.1-linux-amd64.tar.gz -O /helm.tgz && \
    tar -z -x -C /bin -f /helm.tgz --strip-components=1 linux-amd64/helm && \
    rm -f /helm.tgz && \
    mkdir /hooks
COPY --from=addon-operator /app/addon-operator /
COPY --from=addon-operator /app/shell-operator/frameworks /
WORKDIR /
ENV MODULES_DIR /modules
ENV GLOBAL_HOOKS_DIR /global-hooks
ENTRYPOINT ["/sbin/tini", "--", "/addon-operator"]

