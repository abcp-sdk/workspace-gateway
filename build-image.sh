#!/usr/bin/env bash
# Build and push the workspace-gateway image WITHOUT a local build daemon.
# Pipeline: buildkitd (in-cluster) -> docker archive -> skopeo -> the deployment
# registry. The image host is **artifact** (the platform's single OCI registry),
# where the gateway PULLS sandbox/base images from; the old Forgejo `git.agent`
# path no longer holds these images.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# artifact is the single image host (source of base images AND push target).
REGISTRY="${REGISTRY:-artifact.worker.svc.cluster.local}"
NAMESPACE="${NAMESPACE:-coding-workspace}"
NAME="${NAME:-workspace-gateway}"
TAG="${TAG:-$(date +%Y%m%d%H%M%S)}"
DEST="${REGISTRY}/${NAMESPACE}/${NAME}:${TAG}"
BUILDKIT="${BUILDKIT_ADDR:-tcp://buildkitd.agent.svc.cluster.local:1234}"
# artifact write credential (root + the shared artifact token).
ARTIFACT_USER="${ARTIFACT_USER:-root}"
ARTIFACT_TOKEN="${ARTIFACT_TOKEN:-dev-artifact-token}"
PROXY="${PROXY:-http://mihomo.develop.svc.cluster.local:7890}"
# Modules are fetched from the in-cluster artifact registry (proxy.golang.org is
# unreachable from the cluster). Override GOPROXY to build against another source.
GOPROXY="${GOPROXY:-http://artifact.worker.svc.cluster.local/artifacts/go}"
DOCKERFILE="Dockerfile"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "Building ${NAME} image -> ${DEST} (buildkitd=${BUILDKIT})"
buildctl --addr "${BUILDKIT}" build \
  --frontend dockerfile.v0 \
  --local "context=${DIR}" \
  --local "dockerfile=${DIR}" \
  --opt "filename=${DOCKERFILE}" \
  --opt "build-arg:REGISTRY=${REGISTRY}/root" \
  --opt "build-arg:HTTP_PROXY=${PROXY}" \
  --opt "build-arg:HTTPS_PROXY=${PROXY}" \
  --opt "build-arg:NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc,10.199.64.20" \
  --opt "build-arg:GOPROXY=${GOPROXY}" \
  --opt "build-arg:GOSUMDB=off" \
  --output "type=docker,name=${NAMESPACE}/${NAME}:${TAG},dest=${WORK}/image.tar" \
  --progress plain

echo "Pushing to ${DEST}"
skopeo copy \
  --dest-creds "${ARTIFACT_USER}:${ARTIFACT_TOKEN}" \
  --dest-tls-verify=false \
  "docker-archive:${WORK}/image.tar:${NAMESPACE}/${NAME}:${TAG}" \
  "docker://${DEST}"

echo "Verifying push:"
skopeo inspect --creds "${ARTIFACT_USER}:${ARTIFACT_TOKEN}" --tls-verify=false "docker://${DEST}" >/dev/null 2>&1 \
  && echo "OK ${DEST}" \
  || echo "inspect failed for ${DEST} (image may still be present)"
