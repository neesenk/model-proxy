#!/bin/sh
# restart_serve.sh — atomically restart a running `model-proxy serve` in ONE
# invocation (SIGINT -> wait for port release -> nohup start -> wait for listen).
#
# Why atomicity matters (docs/engineering/pitfalls.md #24b, CLI.md「手动重启」):
# on dev machines this proxy is often the coding agent's own LLM gateway.
# Splitting kill and start into two tool calls/terminal steps leaves a downtime
# window in which the agent's next model call fails with Connection error —
# it can no longer even generate the start command. Self-lock. Never split.
#
# Usage:
#   scripts/restart_serve.sh [--build] [--port PORT] [--log PATH] [--dir REPO]
#
#   --build      rebuild first: go build -o model-proxy.new . && mv (building
#                never touches the running process)
#   --port PORT  listen port to gate on (default: parsed from config.yaml's
#                `listen:` in --dir)
#   --log PATH   stdout/stderr append target (default: serve.log in --dir)
#   --dir REPO   repo/binary directory (default: script's parent dir)
#
# Exit codes: 0 restarted (or started fresh); 1 usage/timeout/build failure.

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
PORT=
LOG=
DO_BUILD=0
STOP_TIMEOUT_S=15    # graceful drain budget is 8s, plus margin
START_TIMEOUT_S=60   # first request may trigger catalog refresh etc.

while [ $# -gt 0 ]; do
  case "$1" in
    --build) DO_BUILD=1 ;;
    --port)  PORT=$2; shift ;;
    --log)   LOG=$2; shift ;;
    --dir)   DIR=$2; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1 (see --help)" >&2; exit 1 ;;
  esac
  shift
done

LOG=${LOG:-"$DIR/serve.log"}

if [ -z "$PORT" ]; then
  # listen: "127.0.0.1:15722"  ->  15722 (last colon-separated field)
  PORT=$(sed -n 's/^listen:[[:space:]]*["'"'"']*.*:\([0-9][0-9]*\)["'"'"']*[[:space:]]*$/\1/p' \
    "$DIR/config.yaml" | head -n1)
fi
if [ -z "$PORT" ]; then
  echo "cannot determine listen port from $DIR/config.yaml; pass --port" >&2
  exit 1
fi

port_listening() { lsof -tiTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; }

# 1) optional build — never disturbs the running process
if [ "$DO_BUILD" -eq 1 ]; then
  (cd "$DIR" && go build -o model-proxy.new . && mv model-proxy.new model-proxy)
fi

# 2) SIGINT current listeners (graceful: HTTP drain + final flush)
PIDS=$(lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)
if [ -n "$PIDS" ]; then
  kill -INT $PIDS
fi

# 3) wait for the port to be released. Wall-clock budget (date +%s), not an
#    iteration count: the sleep below falls back to `sleep 1` on /bin/sh
#    implementations without fractional sleep, which would silently multiply
#    any iteration-based budget by 10.
deadline=$(( $(date +%s) + STOP_TIMEOUT_S ))
while port_listening; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "timeout: port $PORT still listening ${STOP_TIMEOUT_S}s after SIGINT" >&2
    exit 1
  fi
  sleep 0.1 2>/dev/null || sleep 1   # some /bin/sh sleep lack fractions
done

# 4) start and wait for the port to come back — same invocation, no gap for
#    a second tool call to creep into
cd "$DIR"
nohup ./model-proxy serve >> "$LOG" 2>&1 &

deadline=$(( $(date +%s) + START_TIMEOUT_S ))
while ! port_listening; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "timeout: serve did not listen on $PORT within ${START_TIMEOUT_S}s; see $LOG" >&2
    exit 1
  fi
  sleep 0.1 2>/dev/null || sleep 1
done

echo "model-proxy serve restarted on port $PORT (log: $LOG)"
