#!/usr/bin/env bash
# xdev-watch.sh — keep other xdev sessions running.
#
# Each sweep lists the xdev agents Herdr recognizes and reads the tail of the
# ones sitting at their prompt. A run that ENDED its turn on an API / stream /
# transport error is crashed, not finished: type "continue" into it.
#
# Two facts make the detection cheap and safe:
#   - xdev emits agent_end on every outcome (internal/agent/loop.go carries
#     payload.error) and the user's hooks report that as herdr "idle". So an
#     errored run is ALWAYS idle — a "working" pane is mid-turn, its screen tail
#     may quote a transient error the retry ladder already recovered from, and it
#     must never be touched.
#   - the mailbox can't wake such a run: a session only polls its inbox while a
#     turn is running (cmd/xdev/inbox.go leaves a message UNREAD while its sink
#     is idle). The dead run has to be revived from its own terminal.
#
# Deliberate ceilings:
#   - ponytail: the marker is a regex over the last TAIL_LINES rendered rows, so
#     a finished run whose final message merely *quotes* "stream error" reads as
#     down. The grace sweep + per-pane nudge cap bound that false positive, and
#     one extra "continue" costs a turn, not work. Upgrade path: have an
#     agent_end hook append payload .error to a spool file and watch that
#     instead of the screen.
#   - one nudge per sweep per pane; never nudge the watcher's own pane.
#
# Usage: scripts/xdev-watch.sh [--once] [--dry-run] [--interval S] [--lines N]
#   --once      one sweep, then exit (for cron / launchd)
#   --dry-run   report what would be nudged; send nothing
#   --interval  sweep period in seconds (default 30)
#   --lines     transcript tail depth per pane (default 12)
set -euo pipefail

INTERVAL=30
TAIL_LINES=12 # the TUI's input box + status line occupy the last ~6 rows
GRACE=1       # sweeps a marker must survive before the first nudge
MAX_NUDGE=6   # nudges per pane before declaring it needs a human

ONCE=0
DRY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --once) ONCE=1 ;;
    --dry-run) DRY=1 ;;
    --interval) INTERVAL="${2:?--interval needs a value}"; shift ;;
    --lines) TAIL_LINES="${2:?--lines needs a value}"; shift ;;
    -h | --help)
      sed -n '2,33p' "$0"
      exit 0
      ;;
    *)
      echo "unknown flag: $1" >&2
      exit 2
      ;;
  esac
  shift
done

command -v herdr >/dev/null || { echo "herdr not in PATH" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq not in PATH" >&2; exit 1; }

# How a provider death reaches the screen: cmd/xdev/tui.go renders
# "stream error: <err>" and "error: <err>"; internal/ai/errors.go classifies the
# transport wording below, which arrives verbatim inside those lines.
DOWN_RE='stream error|api error|API error|error code [0-9]{3}|overloaded|rate.?limit|connection (reset|refused|closed)|TLS handshake|unexpected EOF|EOF while reading|dial tcp|no such host|context deadline exceeded|[Ee]rror: .*(stream|timeout|deadline|unavailable|reset|refused)'

STATE_DIR="${XDEV_WATCH_STATE:-${TMPDIR:-/tmp}/xdev-watch}"
mkdir -p "$STATE_DIR"

log() {
  printf '%s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*" | tee -a "$STATE_DIR/watch.log"
}
count() { cat "$1" 2>/dev/null || echo 0; }

sweep() {
  if ! agents=$(herdr agent list 2>/dev/null); then
    log "skip: herdr agent list failed"
    return
  fi
  rows=$(printf '%s' "$agents" | jq -r '
    .result.agents[]? | select(.agent == "xdev")
    | [(.pane_id // ""), (.agent_status // "unknown")] | join(" ")')

  : >"$STATE_DIR/live"
  while read -r pane status; do
    [ -n "$pane" ] || continue
    printf '%s\n' "$pane" >>"$STATE_DIR/live"
    case "$status" in
      idle | done) ;;
      *) continue ;; # working/blocked/unknown: never steer a live or parked run
    esac
    [ "$pane" = "${HERDR_PANE_ID:-}" ] && continue # the watcher's own pane

    tail_txt=$(herdr pane read --source recent --lines "$TAIL_LINES" "$pane" 2>/dev/null |
      tr -d '\r' | sed -e 's/[[:space:]]*$//' | grep -v '^$' || true)
    marker=$(printf '%s\n' "$tail_txt" | grep -iE "$DOWN_RE" | tail -1 || true)
    if [ -z "$marker" ]; then
      rm -f "$STATE_DIR/strike.$pane" "$STATE_DIR/nudge.$pane"
      continue
    fi

    strike=$(( $(count "$STATE_DIR/strike.$pane") + 1 ))
    echo "$strike" >"$STATE_DIR/strike.$pane"
    brief=$(printf '%s' "$marker" | tr -s ' ' | cut -c1-140)
    if [ "$strike" -le "$GRACE" ]; then
      log "watch $pane: dead-turn marker (strike $strike, re-checking): $brief"
      continue
    fi

    nudge=$(( $(count "$STATE_DIR/nudge.$pane") + 1 ))
    echo "$nudge" >"$STATE_DIR/nudge.$pane"
    if [ "$nudge" -gt "$MAX_NUDGE" ]; then
      log "give up $pane: $nudge nudges with no recovery; needs a human"
      continue
    fi
    if [ "$DRY" = "1" ]; then
      log "DRY-RUN would send 'continue' to $pane (nudge $nudge): $brief"
      continue
    fi
    # `agent prompt` refuses a blocked agent and fails once the agent exited —
    # both are "leave it alone", reported rather than swallowed.
    if herdr agent prompt "$pane" "continue" >/dev/null 2>&1; then
      log "sent 'continue' to $pane (nudge $nudge): $brief"
    else
      log "nudge refused for $pane (herdr: blocked, exited, or stalled)"
    fi
  done <<EOF
$rows
EOF
  # Counters of panes that vanished since the last sweep are dropped.
  for f in "$STATE_DIR"/strike.* "$STATE_DIR"/nudge.*; do
    [ -e "$f" ] || continue
    pane=${f##*/}
    pane=${pane#*.}
    grep -qxF "$pane" "$STATE_DIR/live" || rm -f "$f"
  done
}

if [ "$ONCE" = "1" ]; then
  sweep
  exit 0
fi

log "watching xdev panes every ${INTERVAL}s (dry-run=$DRY, log: $STATE_DIR/watch.log)"
while :; do
  sweep
  sleep "$INTERVAL"
done
