#!/usr/bin/env bash
# Build ONE workspace image, with output redirected to a log (never truncated).
#
#   ./build-one.sh toolchain base      # toolchain-base
#   ./build-one.sh toolchain node      # toolchain-node
#   ./build-one.sh toolchain clang     # toolchain-clang
#
# Logs to /tmp/wsbuild/<stage>-<name>.log; run it in the background and poll.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

STAGE="${1:?usage: build-one.sh toolchain <name>}"
NAME="${2:-}"

export NO_PROXY="${NO_PROXY:-localhost,127.0.0.1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,.svc.cluster.local,.svc,.fenjin.org,.nip.io,10.199.64.20}"
export no_proxy="${no_proxy:-$NO_PROXY}"
export BUILDKIT_ADDR="${BUILDKIT_ADDR:-tcp://buildkitd.agent.svc.cluster.local:1234}"

LOG_DIR="${LOG_DIR:-/tmp/wsbuild}"
mkdir -p "$LOG_DIR"

case "$STAGE" in
  toolchain) LOG="${LOG_DIR}/toolchain-${NAME}.log"; CMD=("${HERE}/build-toolchain.sh" "$NAME") ;;
  *) echo "unknown stage: $STAGE" >&2; exit 2 ;;
esac

: > "$LOG"
echo "building ${STAGE} ${NAME} -> ${LOG}"
setsid bash -c "'${CMD[0]}' $(printf '%q ' "${CMD[@]:1}") >> '${LOG}' 2>&1; echo \"EXIT=\$?\" >> '${LOG}'" < /dev/null &
echo "pid=$! log=$LOG"
