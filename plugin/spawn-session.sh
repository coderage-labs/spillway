#!/usr/bin/env bash
# spillway spawn-session — start a detached Claude Code session with Remote
# Control so it can be driven from the phone/web app.
#
# Server mode (`claude remote-control`) is deliberate: interactive
# `--remote-control --resume` mints a REPLACEMENT session without history when
# reattach fails, while server mode re-serves the same server-side session
# within ~4h of shutdown (design doc §6.17).
#
# tmux is preferred (attachable locally later); nohup is the fallback.
set -euo pipefail

usage() { echo "usage: spawn-session <name> [cwd] [-- <claude args>]" >&2; exit 2; }
[ $# -ge 1 ] || usage

NAME="$1"; shift
CWD="${1:-$PWD}"
[ $# -gt 0 ] && shift || true
[ "${1:-}" = "--" ] && shift || true

[ -d "$CWD" ] || { echo "spawn-session: no such directory: $CWD" >&2; exit 1; }

# Refuse to launch unproxied: a session that bypasses the pool silently spends
# one account's quota with no rotation.
# Indentation under `proxy:` varies by YAML writer, so match on the key alone.
CFG="${SPILLWAY_CONFIG:-$HOME/.config/spillway.yaml}"
PORT="$(awk '/^[[:space:]]+port:[[:space:]]*[0-9]+/{print $2; exit}' "$CFG" 2>/dev/null || true)"
[ -n "${PORT:-}" ] || PORT=7654
if ! nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
  echo "spawn-session: spillway is not listening on 127.0.0.1:$PORT — start it with \`spillway service install\` or \`spillway server\`" >&2
  exit 1
fi

# Resolve the binary: explicit override, then PATH, then a build sitting in
# the repo next to this script (the common "not installed yet" case).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${SPILLWAY_BIN:-}"
if [ -z "$BIN" ]; then
  if command -v spillway >/dev/null 2>&1; then
    BIN="$(command -v spillway)"
  elif [ -x "$SCRIPT_DIR/../spillway" ]; then
    BIN="$SCRIPT_DIR/../spillway"
  else
    echo "spawn-session: spillway binary not found — install it on PATH (go install ./cmd/spillway) or set SPILLWAY_BIN" >&2
    exit 1
  fi
fi

LOG="${TMPDIR:-/tmp}/spillway-session-$NAME.log"

if command -v tmux >/dev/null 2>&1; then
  tmux new-session -d -s "spillway-$NAME" -c "$CWD" \
    "'$BIN' run -- remote-control --remote-control-session-name-prefix '$NAME' $* 2>&1 | tee '$LOG'"
  echo "spawned in tmux session spillway-$NAME (attach: tmux attach -t spillway-$NAME)"
else
  ( cd "$CWD" && nohup "$BIN" run -- remote-control \
      --remote-control-session-name-prefix "$NAME" "$@" >"$LOG" 2>&1 & )
  echo "spawned detached (no tmux; install it to attach locally)"
fi

echo "log: $LOG"
echo "the session appears in the Claude mobile/web app shortly; it routes through the spillway pool"
