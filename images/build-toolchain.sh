#!/usr/bin/env bash
# Stage 1: build the WORKSPACE generic toolchain images and push them to the
# workspace registry. No easyworker, no CA — plain dev images.
#
#   DISTRO=debian-trixie ./build-toolchain.sh node
#   ./build-toolchain.sh            # base + every WORKSPACE_LANGS entry
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
. "${HERE}/config.sh"

DIR="${HERE}/toolchain/${DISTRO}"
[ -d "$DIR" ] || { echo "no toolchain dir for DISTRO=${DISTRO}: ${DIR}"; exit 1; }

LANGS="${*:-base ${WORKSPACE_LANGS}}"
TOOLCHAIN_BASE_REF="${REGISTRY}/${NAMESPACE}/${TOOLCHAIN_REPO}-base:${TOOLCHAIN_TAG}"

build() { # proto
  local proto="$1" name df
  if [ "$proto" = "base" ]; then
    name="${TOOLCHAIN_REPO}-base"
    df="${DIR}/base.Dockerfile"
    build_image "$name" "$df" "$TOOLCHAIN_TAG" "$DISTRO_BASE"
    return
  fi
  local dfproto="$proto"; [ "$proto" = "go" ] && dfproto="golang"
  name="${TOOLCHAIN_REPO}-${proto}"
  df="${DIR}/Dockerfile.${dfproto}"
  [ -f "$df" ] || { echo "no Dockerfile for ${proto} in ${DISTRO}: ${df}"; exit 1; }
  # A `<lang>.base` overrides the parent image:
  #   * `@distro`            -> the raw distro base (e.g. debian:trixie-slim)
  #   * contains ':' or '/'  -> an explicit image ref, used verbatim
  #   * otherwise            -> a sibling toolchain (kotlin/scala -> java25)
  local parent="$TOOLCHAIN_BASE_REF"
  if [ -f "${DIR}/${dfproto}.base" ]; then
    local p; p="$(grep -vE '^[[:space:]]*(#|$)' "${DIR}/${dfproto}.base" | head -1)"
    [ -n "$p" ] || { echo "empty parent in ${DIR}/${dfproto}.base"; exit 1; }
    case "$p" in
      @distro)          parent="$DISTRO_BASE" ;;
      *:*|*/*)          parent="$p" ;;
      *)                parent="${REGISTRY}/${NAMESPACE}/${TOOLCHAIN_REPO}-${p}:${TOOLCHAIN_TAG}" ;;
    esac
  fi
  build_image "$name" "$df" "$TOOLCHAIN_TAG" "$parent"
}

# java25 is the parent of kotlin/scala; build it first when either is requested.
case " ${LANGS} " in
  *" kotlin "*|*" scala "*) case " ${LANGS} " in *" java25 "*) ;; *) LANGS="java25 ${LANGS}" ;; esac ;;
esac
for l in ${LANGS}; do build "$l"; done
