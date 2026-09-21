#!/usr/bin/env bash
# Sync the vendored agent.v1 proto from the authoritative source (agent-proto)
# and regenerate the gateway's Go + ES bindings.
#
# The gateway vendors agent.v1 because it generates a SMALLER, gateway-specific
# surface (workspace.v1 imports agent.v1 message types) and must not drift from
# the agent server's contract. This script is the one command that keeps it in
# sync; run it whenever agent-proto changes.
#
# Usage:
#   ./scripts/sync-agent-proto.sh [--check]
#
#   --check   regenerate to a temp dir and diff; write nothing.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GW="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT="$(cd "$GW/.." && pwd)"
SRC="$ROOT/agent-proto/proto/agent/v1/agent.proto"
DST="$GW/proto/agent/v1/agent.proto"
GO_PKG_UPSTREAM='github.com/abcp-sdk/agent-sdk-go/agent/v1;agentv1'
GO_PKG_GATEWAY='github.com/abcp-sdk/workspace-gateway/gen/agent/v1;agentv1'

CHECK=0
[ "${1:-}" = "--check" ] && CHECK=1

[ -f "$SRC" ] || { echo "missing source proto: $SRC" >&2; exit 2; }

# 1. Rewrite only the go_package option to target this module.
REWRITTEN="$(mktemp)"
trap 'rm -f "$REWRITTEN"' EXIT
sed "s#${GO_PKG_UPSTREAM}#${GO_PKG_GATEWAY}#" "$SRC" > "$REWRITTEN"

if [ "$CHECK" = 1 ]; then
  if [ -f "$DST" ] && diff -q "$REWRITTEN" "$DST" >/dev/null 2>&1; then
    echo "agent.proto: in sync with agent-proto"
    exit 0
  fi
  echo "agent.proto: DIFFERS from agent-proto" >&2
  diff "$DST" "$REWRITTEN" >&2 || true
  exit 1
fi

cp "$REWRITTEN" "$DST"
echo "synced: $SRC -> $DST"

# 2. Regenerate Go + ES bindings (remote buf plugins).
( cd "$GW/proto" && buf generate --template buf.gen.workspace.yaml )
( cd "$GW/proto" && buf generate --template buf.gen.es.yaml )
echo "regenerated: gen/agent/v1, gen/workspace/v1, gen/es/*"
