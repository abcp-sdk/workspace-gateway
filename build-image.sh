#!/usr/bin/env bash
# Build and push the workspace-gateway image WITHOUT a local build daemon.
# Mirrors the other abcp-sdk build-image.sh scripts: buildkitd -> docker
# archive -> skopeo -> forgejo OCI.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

REGISTRY="${REGISTRY:-git.agent.svc.cluster.local}"
NAMESPACE="${NAMESPACE:-abcp}"
NAME="${NAME:-workspace-gateway}"
TAG="${TAG:-$(date +%Y%m%d%H%M%S)}"
DEST="${REGISTRY}/${NAMESPACE}/${NAME}:${TAG}"
BUILDKIT="${BUILDKIT_ADDR:-tcp://buildkitd.temp.svc.cluster.local:1234}"
FORGEJO_USER="${FORGEJO_USER:-root}"
FORGEJO_PASS="${FORGEJO_PASS:-devpassword}"
PROXY="${PROXY:-http://mihomo.develop.svc.cluster.local:7890}"
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
  --output "type=docker,name=${NAMESPACE}/${NAME}:${TAG},dest=${WORK}/image.tar" \
  --progress plain

echo "Pushing to forgejo ${DEST}"
skopeo copy \
  --dest-creds "${FORGEJO_USER}:${FORGEJO_PASS}" \
  --dest-tls-verify=false \
  "docker-archive:${WORK}/image.tar:${NAMESPACE}/${NAME}:${TAG}" \
  "docker://${DEST}"

echo "Verifying push:"
skopeo inspect --creds "${FORGEJO_USER}:${FORGEJO_PASS}" --tls-verify=false "docker://${DEST}" >/dev/null 2>&1 \
  && echo "OK ${DEST}" \
  || echo "inspect failed for ${DEST} (image may still be present)"
