#!/usr/bin/env bash
# Shared knobs for the WORKSPACE service's toolchain base images.
#
# This tree is a workspace-owned COPY of easyworker's stage-1 toolchain build
# (see /home/user/easylab-platform/easyworker/images). It differs on purpose:
#
#   * Registry/namespace default to the workspace stack
#     (git.agent.fenjin.org/agent-toolchain) with NATIVE `<lang>:<distro>` tags.
#   * NO egress MITM CA and NO worker binary are baked in. The workspace has no
#     easylab egress sidecar, and the gateway injects easyworker into ANY base
#     image at sandbox launch time (derive-on-launch). These are plain dev
#     toolchain images.
#
# The gateway's list-oci-images default (owner=agent-toolchain) surfaces exactly
# this set; it is a convenience, not a restriction (any image can be a sandbox).
#
#   DISTRO=debian-trixie ./build-toolchain.sh node
#   ./build-toolchain.sh clang       # conan + clang + libc++ + llvm
#
# Sourced (not executed) by the build scripts.

DISTRO="${DISTRO:-debian-trixie}"
case "$DISTRO" in
  debian-trixie) DISTRO_TAG="${DISTRO_TAG:-debian-trixie}"; DISTRO_BASE="${DISTRO_BASE:-debian:trixie-slim}" ;;
  *) echo "workspace toolchain images currently support DISTRO=debian-trixie only" >&2; exit 1 ;;
esac

REGISTRY="${REGISTRY:-git.agent.svc.cluster.local}"
# The toolchain base images live under a dedicated org namespace so the
# gateway's list-oci-images default (agent-toolchain) finds them.
NAMESPACE="${NAMESPACE:-agent-toolchain}"
BUILDKIT="${BUILDKIT_ADDR:-tcp://buildkitd.agent.svc.cluster.local:1234}"
BUILD_PROXY="${BUILD_PROXY:-http://mihomo.develop.svc.cluster.local:7890}"
FORGEJO_USER="${FORGEJO_USER:-root}"
FORGEJO_PASS="${FORGEJO_PASS:-devpassword}"

# The registry and buildkitd MUST bypass the HTTP proxy: skopeo pushing to
# git.agent.fenjin.org through mihomo returns a bogus 530, and buildkitd is an
# in-cluster ClusterIP. Force these onto NO_PROXY regardless of the ambient
# value (which here does NOT contain fenjin.org).
for _h in git.agent.fenjin.org .fenjin.org .nip.io 10.199.64.20; do
  case ",${NO_PROXY}," in *",${_h},"*) ;; *) NO_PROXY="${NO_PROXY:+${NO_PROXY},}${_h}" ;; esac
done
no_proxy="${NO_PROXY}"
export NO_PROXY no_proxy

# Stage-1 generic toolchain images.
TOOLCHAIN_REPO="${TOOLCHAIN_REPO:-toolchain}"
TOOLCHAIN_TAG="${TOOLCHAIN_TAG:-${DISTRO_TAG}}"

# The workspace catalog's language set (the curated subset). `clang` is the
# C/C++ toolchain (conan + clang + libc++ + llvm); it is the one image built
# from the raw distro base (via clang.base = @distro) instead of toolchain-base,
# because it deliberately ships WITHOUT gcc.
WORKSPACE_LANGS="${WORKSPACE_LANGS:-node python go rust java kotlin scala dart dotnet elixir php ruby swift zig clang}"

# Artifact cache. The workspace cache hardlinks the easyworker one and adds the
# versions the workspace pins (node 26.9.0, JDK 26, sbt 1.13.0, zig 0.16.0).
CACHE_ROOT="${CACHE_ROOT:-${HERE}/cache}"

# build_image <repo-name> <dockerfile-fragments> <tag> <base-image-ref> [extra-context...]
# Runs in a subshell; aborts the sourcing script on failure.
build_image() (
  set -euo pipefail
  local name="$1" df="$2" tag="$3" base="$4"
  shift 4
  local work ctx f frag
  work="$(mktemp -d)"; ctx="${work}/ctx"; mkdir -p "${ctx}"
  trap 'rm -rf "${work}"' EXIT
  if [ -f "${df}" ]; then
    cp "${df}" "${ctx}/Dockerfile"
  else
    : > "${ctx}/Dockerfile"
    for frag in ${df}; do
      [ -f "${frag}" ] || { echo "missing Dockerfile fragment: ${frag}"; exit 1; }
      cat "${frag}" >> "${ctx}/Dockerfile"
    done
  fi
  for f in "$@"; do
    [ -f "${HERE}/${f}" ] || { echo "missing build context file: ${HERE}/${f}"; exit 1; }
    cp "${HERE}/${f}" "${ctx}/${f}"
  done
  # Copy only the cached artifacts the Dockerfile references.
  local cf cache_dir="${CACHE_ROOT}/${DISTRO}"
  while read -r cf; do
    [ -n "$cf" ] || continue
    if [ ! -f "${cache_dir}/${cf}" ]; then
      echo "missing cached artifact: ${cache_dir}/${cf}" >&2
      exit 1
    fi
    mkdir -p "${ctx}/cache"
    cp "${cache_dir}/${cf}" "${ctx}/cache/${cf}"
  done < <(sed -n 's#^COPY cache/\([^ ]*\).*#\1#p' "${ctx}/Dockerfile")

  local dest="${REGISTRY}/${NAMESPACE}/${name}:${tag}"
  echo "== build ${name}:${tag} on ${BUILDKIT} (from ${base}) =="
  buildctl --addr "${BUILDKIT}" build \
    --frontend dockerfile.v0 \
    --local "context=${ctx}" \
    --local "dockerfile=${ctx}" \
    --opt "filename=Dockerfile" \
    --opt "build-arg:BASE_IMAGE=${base}" \
    --output "type=docker,name=${name}:${tag},dest=${work}/image.tar" \
    --progress plain
  echo "== push ${dest} =="
  skopeo copy --dest-creds "${FORGEJO_USER}:${FORGEJO_PASS}" --dest-tls-verify=false \
    "docker-archive:${work}/image.tar:${name}:${tag}" "docker://${dest}"
  skopeo inspect --creds "${FORGEJO_USER}:${FORGEJO_PASS}" --tls-verify=false "docker://${dest}" >/dev/null \
    && echo "OK ${dest}" || { echo "push verify failed"; exit 1; }
)
