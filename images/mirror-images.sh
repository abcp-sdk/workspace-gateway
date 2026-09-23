#!/usr/bin/env bash
# Mirror the pinned SHARED MIDDLEWARE images (images/middleware.env) into the
# workspace registry under the shared `${NAMESPACE}` (default agent-toolchain).
#
#   ./mirror-images.sh            # mirror every entry in middleware.env
#   ./mirror-images.sh postgres   # mirror just one (case-insensitive name)
#
# Each image is re-served with a no-op `FROM <upstream>` build (its ENTRYPOINT/
# CMD/ENV/USER/EXPOSE/WORKDIR are preserved). docker.io is reached through the
# buildkitd HTTP proxy; the push bypasses it (NO_PROXY).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=config.sh
. "${HERE}/config.sh"

ENV_FILE="${HERE}/middleware.env"
[ -f "$ENV_FILE" ] || { echo "missing ${ENV_FILE}"; exit 1; }

want="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"

fail=0
while IFS= read -r line; do
  case "$line" in ''|'#'*) continue ;; esac
  name="${line%%:*}"
  ref="${line#*:}"
  [ -n "$name" ] && [ -n "$ref" ] || { echo "bad entry: $line"; fail=1; continue; }
  if [ -n "$want" ] && [ "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" != "$want" ]; then
    continue
  fi
  # The internal tag is the upstream tag (the part after the last ':').
  tag="${ref##*:}"
  echo "=== mirror ${name} <- ${ref} (tag ${tag}) ==="
  if build_image "${name}" "${HERE}/middleware.Dockerfile" "${tag}" "${ref}"; then
    echo "=== OK ${NAME:-}${name} ==="
  else
    echo "=== FAIL ${name} ===" >&2
    fail=1
  fi
done < "$ENV_FILE"

exit "$fail"
