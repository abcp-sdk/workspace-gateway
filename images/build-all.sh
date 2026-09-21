#!/usr/bin/env bash
# Build EVERY workspace image ONE AT A TIME (never in parallel), each with its
# own redirected log. Run detached and poll:
#
#   setsid bash images/build-all.sh > /tmp/wsbuild/all.log 2>&1 < /dev/null &
#   tail -f /tmp/wsbuild/all.log
#
# Builds the toolchain base + every language toolchain (clang included). No
# preset stage: sandboxes inject the worker into ANY image at launch time.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
. "${HERE}/config.sh"
export NO_PROXY no_proxy BUILDKIT_ADDR

LOG_DIR="${LOG_DIR:-/tmp/wsbuild}"
mkdir -p "$LOG_DIR"

run() { # <label> <logfile> <cmd...>
  local label="$1" log="$2"; shift 2
  echo "=== [$(date +%H:%M:%S)] START ${label} -> ${log} ==="
  : > "$log"
  if "$@" >> "$log" 2>&1; then
    echo "=== [$(date +%H:%M:%S)] OK ${label} ==="
  else
    echo "=== [$(date +%H:%M:%S)] FAIL ${label} (see ${log}) ==="
  fi
}

# Base, then every language toolchain (clang included).
run "toolchain-base" "${LOG_DIR}/toolchain-base.log" "${HERE}/build-toolchain.sh" base
for l in ${WORKSPACE_LANGS}; do
  run "toolchain-${l}" "${LOG_DIR}/toolchain-${l}.log" "${HERE}/build-toolchain.sh" "$l"
done

echo "=== ALL DONE [$(date +%H:%M:%S)] ==="
