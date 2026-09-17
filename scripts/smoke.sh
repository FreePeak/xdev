#!/usr/bin/env bash
# Smoke test: xdev print mode against the local onegw gateway.
# Usage: ./scripts/smoke.sh "prompt" [model-ref]
# Requires: the gateway from ~/.xdev/agent/models.yml (default onegw/<first model>).
set -euo pipefail
cd "$(dirname "$0")/.."

PROMPT="${1:-Create a file named smoke.txt containing the text 'xdev was here', then verify it exists by reading it back.}"
MODEL="${2:-}"

echo "== building xdev =="
go build -o /tmp/xdev-smoke ./cmd/xdev

ARGS=()
if [[ -n "$MODEL" ]]; then
  ARGS+=(-model "$MODEL")
fi

echo "== running: $PROMPT =="
# Empty-array expansion must survive `set -u` on macOS bash 3.2.
/tmp/xdev-smoke ${ARGS[@]+"${ARGS[@]}"} "$PROMPT"

echo
echo "== session files written =="
ls -lt ~/.xdev/agent/sessions/ | head -5
