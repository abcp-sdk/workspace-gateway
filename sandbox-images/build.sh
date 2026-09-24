#!/usr/bin/env bash
# Build SANDBOX images by baking the agent-worker binary into an existing
# GENERIC toolchain image (agent-toolchain/toolchain-<lang>:debian-trixie).
# Pushes them to <registry>/<SANDBOX_ORG>/sandbox-<lang>:debian-trixie.
#
# The gateway no longer injects the worker at launch; a sandbox may only run
# one of these dedicated images. Re-run this whenever the worker changes.
#
#   ./build.sh base node python      # build sandbox-base/-node/-python
#   ./build.sh                       # build the default set (base + a few langs)
#
# Env overrides:
#   REGISTRY     registry host            (default git.agent.svc.cluster.local)
#   SANDBOX_ORG  destination org          (default sandbox)
#   TOOLCHAIN_ORG source toolchain org    (default agent-toolchain)
#   DISTRO_TAG   toolchain tag            (default debian-trixie)
#   BUILDKIT_ADDR buildkitd               (default tcp://buildkitd.agent.svc.cluster.local:1234)
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/.." && pwd)"

REGISTRY="${REGISTRY:-git.agent.svc.cluster.local}"
SANDBOX_ORG="${SANDBOX_ORG:-sandbox}"
TOOLCHAIN_ORG="${TOOLCHAIN_ORG:-agent-toolchain}"
DISTRO_TAG="${DISTRO_TAG:-debian-trixie}"
BUILDKIT_ADDR="${BUILDKIT_ADDR:-tcp://buildkitd.agent.svc.cluster.local:1234}"
FORGEJO_USER="${FORGEJO_USER:-root}"
FORGEJO_PASS="${FORGEJO_PASS:-devpassword}"

# The registry + buildkitd MUST bypass any HTTP proxy.
for _h in .svc.cluster.local .svc 10.199.64.20 "${REGISTRY%%:*}"; do
  case ",${NO_PROXY:-}," in *",${_h},"*) ;; *) NO_PROXY="${NO_PROXY:+${NO_PROXY},}${_h}" ;; esac
done
no_proxy="${NO_PROXY}"
export NO_PROXY no_proxy

# Default set: the base plus the common languages.
LANGS="${*:-base node python go java rust}"

echo "==> cross-compiling agent-worker (linux/amd64)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -C "${ROOT}/worker-src" -trimpath -ldflags "-s -w" \
  -o "${HERE}/agent-worker" ./cmd/agent-worker
ls -l "${HERE}/agent-worker"

build_one() {
  local proto="$1" src name dest work
  if [ "$proto" = "base" ]; then
    name="sandbox-base"
  else
    name="sandbox-${proto}"
  fi
  src="${REGISTRY}/${TOOLCHAIN_ORG}/toolchain-${proto}:${DISTRO_TAG}"
  dest="${REGISTRY}/${SANDBOX_ORG}/${name}:${DISTRO_TAG}"
  work="$(mktemp -d)"
  cp "${HERE}/Dockerfile" "${HERE}/agent-worker" "${work}/"
  echo "== build ${name} (FROM ${src}) =="
  buildctl --addr "${BUILDKIT_ADDR}" build \
    --frontend dockerfile.v0 \
    --local "context=${work}" \
    --local "dockerfile=${work}" \
    --opt "filename=Dockerfile" \
    --opt "build-arg:BASE_IMAGE=${src}" \
    --output "type=docker,name=${name}:${DISTRO_TAG},dest=${work}/image.tar" \
    --progress plain
  echo "== push ${dest} =="
  skopeo copy --dest-creds "${FORGEJO_USER}:${FORGEJO_PASS}" --dest-tls-verify=false \
    "docker-archive:${work}/image.tar:${name}:${DISTRO_TAG}" "docker://${dest}"
  rm -rf "${work}"
  echo "OK ${dest}"
}

for l in ${LANGS}; do
  build_one "$l"
done
