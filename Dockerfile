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

FROM ${REGISTRY}/alpine:3.24
# Keep the official CDN (the build runs behind a proxy; swapping to a mirror
# breaks `apk`). `apk` reads the lowercase http(s)_proxy variables.
ARG HTTP_PROXY
ARG HTTPS_PROXY
ENV http_proxy=${HTTP_PROXY} https_proxy=${HTTPS_PROXY}
RUN apk add --no-cache ca-certificates
COPY --from=build /out/workspace-gateway /usr/local/bin/workspace-gateway
COPY --from=buildkit /usr/bin/buildctl /usr/local/bin/buildctl
ENV PORT=8080 GATEWAY_DB=/data/workspace-gateway.db
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/workspace-gateway"]
