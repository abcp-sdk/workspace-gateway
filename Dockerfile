# workspace-gateway Go build (buildkitd -> forgejo OCI). The generated protos
# are vendored under gen/; the k8s client-go deps come from the module proxy.
ARG REGISTRY=git.agent.svc.cluster.local/root
# Declared before any FROM so it is usable in a later FROM line.
ARG BUILDKIT_IMAGE=docker.io/moby/buildkit:v0.32.2-rootless
FROM ${REGISTRY}/golang:1.26-alpine AS build
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc,10.199.64.20 \
    GOWORK=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/workspace-gateway ./cmd/workspace-gateway

# buildctl (from the official rootless BuildKit image) lets the gateway run
# repo image builds against the cluster buildkitd.
FROM ${BUILDKIT_IMAGE} AS buildkit

# easyworker binary, injected into every sandbox base image at launch time
# (derive-on-launch). Built from the vendored copy under worker-src/, so the
# gateway image is self-contained. That copy mirrors abcp-sdk/worker (the
# abcp-sdk-owned fork of easylab-platform/easyworker carrying this gateway's
# worker modifications); keep the two in sync.
FROM ${REGISTRY}/golang:1.26-alpine AS worker
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV HTTP_PROXY=${HTTP_PROXY} HTTPS_PROXY=${HTTPS_PROXY} GOWORK=off CGO_ENABLED=0
WORKDIR /src
COPY worker-src/go.mod worker-src/go.sum ./
RUN go mod download
COPY worker-src/ ./
RUN GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/easyworker ./cmd/easyworker

FROM ${REGISTRY}/alpine:3.24
# Keep the official CDN (the build runs behind a proxy; swapping to a mirror
# breaks `apk`). `apk` reads the lowercase http(s)_proxy variables.
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV http_proxy=${HTTP_PROXY} https_proxy=${HTTPS_PROXY}
RUN apk add --no-cache ca-certificates
COPY --from=build /out/workspace-gateway /usr/local/bin/workspace-gateway
COPY --from=buildkit /usr/bin/buildctl /usr/local/bin/buildctl
COPY --from=worker /out/easyworker /usr/local/lib/easyworker/easyworker
ENV PORT=8080 GATEWAY_DB=/data/workspace-gateway.db \
    WORKER_BIN=/usr/local/lib/easyworker/easyworker
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/workspace-gateway"]
