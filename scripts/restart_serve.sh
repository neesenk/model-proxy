#!/bin/sh
# restart_serve.sh — atomically restart `model-proxy serve` as a SUPERVISED
# DAEMON in ONE invocation, rolling the binary back when the new one fails to
# come up:
#
#   [optional --build: go build -o model-proxy.new .]
#   → cp model-proxy model-proxy.old        (rollback snapshot, only when a
#                                            new binary is being swapped in)
#   → mv model-proxy.new model-proxy        (swap; never touches the running
#                                            process — unix inode rename)
#   → ./model-proxy serve stop               (daemon supervisor, or a
#                                            pidfile-owning foreground serve;
#                                            graceful SIGTERM → SIGKILL
#                                            escalation, stale-pid cleanup)
#     + SIGINT remaining port listeners → wait port release (SIGKILL
#       stragglers) — manual no-pidfile serves and wedged processes
#   → ./model-proxy serve daemon --log-file <log>
#   → wait port listening, re-check after a 1s settle
#   → on failure: serve stop the failed daemon (a live pidfile would make the
#     rollback daemonize refuse), mv model-proxy.old back, daemon again —
#     exit 1 with "rolled back" (previous binary serving); if the rollback
#     start fails too, exit 1 with the port DOWN and the old binary restored
#     on disk.
#
# Why atomicity matters (docs/engineering/pitfalls.md #24b, CLI.md「手动重启」):
# on dev machines this proxy is often the coding agent's own LLM gateway.
# Splitting stop and start into two tool calls/terminal steps leaves a
# downtime window in which the agent's next model call fails with Connection
# error — it can no longer even generate the start command. Self-lock. Never
# split.
#
# Why daemon mode: the supervisor restarts a crashed worker (1s→30s backoff),
# so a runtime crash no longer waits for the next manual restart, and
# `serve stop` / `serve status` / login-logout SIGHUP all find the daemon via
# the pidfile beside the log file (set log_file in config.yaml — or always
# restart through this script — so every derivation agrees on one location).
#
# Usage:
#   scripts/restart_serve.sh [--build] [--port PORT] [--log PATH] [--dir REPO]
#
#   --build      rebuild first: go build -o model-proxy.new . (building never
#                touches the running process), then snapshot + swap
#   --port PORT  listen port to gate on (default: parsed from config.yaml's
#                `listen:` in --dir)
#   --log PATH   daemon log file, passed as `serve daemon --log-file`
#                (default: serve.log in --dir; the daemon pidfile lands
#                beside it: serve.pid)
#   --dir REPO   repo/binary directory (default: script's parent dir)
#
# Exit codes: 0 restarted (or started fresh); 1 usage/timeout/build failure,
#   or the new binary failed and the PREVIOUS binary was rolled back and is
#   serving (the message says which — a 1 here means "not deployed", not
#   "gateway down").

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
PORT=
LOG=
DO_BUILD=0
STOP_TIMEOUT_S=15    # graceful drain budget is 8s, plus margin
START_TIMEOUT_S=60   # first request may trigger catalog refresh etc.
SETTLE_S=1           # re-check window after the port first listens

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

wait_port_down() {
  deadline=$(( $(date +%s) + STOP_TIMEOUT_S ))
  while port_listening; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      return 1
    fi
    sleep 0.1 2>/dev/null || sleep 1   # some /bin/sh sleep lack fractions
  done
  return 0
}

wait_port_up() {
  deadline=$(( $(date +%s) + START_TIMEOUT_S ))
  while ! port_listening; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      return 1
    fi
    sleep 0.1 2>/dev/null || sleep 1
  done
  # Settle re-check: a worker that crashes immediately after binding (the
  # supervisor would respawn it into another bind-fail loop) must not count
  # as a successful start.
  sleep "$SETTLE_S"
  port_listening
}

# 1) optional build — never disturbs the running process. A failed build must
#    not leave a partial model-proxy.new behind: the next no-build run would
#    swap the stale artifact in.
if [ "$DO_BUILD" -eq 1 ]; then
  if ! (cd "$DIR" && go build -o model-proxy.new .); then
    rm -f "$DIR/model-proxy.new"
    echo "build failed — running binary untouched" >&2
    exit 1
  fi
fi

cd "$DIR"

# 2) binary snapshot + swap. The snapshot exists ONLY when a new binary is
# being swapped in: a plain restart (no --build) has nothing to roll back to —
# model-proxy.old always names the binary that ran BEFORE the last --build
# swap, never a copy of whatever is on disk now.
if [ -f model-proxy.new ]; then
  if [ -f model-proxy ]; then
    cp model-proxy model-proxy.old
  fi
  mv model-proxy.new model-proxy
fi
if [ ! -f model-proxy ]; then
  echo "no binary at $DIR/model-proxy (run with --build)" >&2
  exit 1
fi

# 3) stop whatever owns the port.
# 3a) `serve stop` handles the graceful paths: the daemon supervisor (SIGTERM
# → forwards to worker, 10s budget, SIGKILL) and a pidfile-owning foreground
# serve. Two derivations are probed — the script-managed daemon's pidfile
# (beside $LOG) and the config log_file / $TMPDIR default (what a foreground
# serve or an older daemon claimed). Both exit 0 when nothing is found.
# An ancient binary without the subcommand (or a config-fatal stop) falls
# through to the manual pidfile stop below. Both probes run unconditionally:
# `serve stop` exits 0 when nothing is found, so a `||` chain would skip the
# second location whenever the first probe finds nothing.
./model-proxy serve stop --log-file "$LOG" >/dev/null 2>&1 || true
./model-proxy serve stop >/dev/null 2>&1 || true
# 3b) manual pidfile fallback: config log_file-derived path (default
# $TMPDIR/model-proxy.pid), with the identity check — a stale pidfile may name
# a reused, foreign pid; only ever signal a process that is actually a
# model-proxy.
LOG_FILE=$(sed -n 's/^log_file:[[:space:]]*["'"'"']*\([^"'"'"']*\)["'"'"']*[[:space:]]*$/\1/p' \
  "$DIR/config.yaml" | head -n1)
if [ -z "$LOG_FILE" ]; then
  PIDFILE="${TMPDIR:-/tmp}/model-proxy.pid"
else
  case "$LOG_FILE" in
    "~/"*) LOG_FILE="$HOME/${LOG_FILE#"~/"}" ;;
  esac
  case "$LOG_FILE" in
    /*) : ;;
    *)  LOG_FILE="$DIR/$LOG_FILE" ;;
  esac
  case "$LOG_FILE" in
    *.log) PIDFILE="${LOG_FILE%.log}.pid" ;;
    *)     PIDFILE="$LOG_FILE.pid" ;;
  esac
fi
if [ -f "$PIDFILE" ]; then
  SPID=$(sed -n 's/^\([0-9][0-9]*\).*$/\1/p' "$PIDFILE" | head -n1)
  if [ -n "$SPID" ] && kill -0 "$SPID" 2>/dev/null && \
     [ "$(ps -p "$SPID" -o comm= 2>/dev/null | sed 's:.*/::')" = "model-proxy" ]; then
    kill -TERM "$SPID" 2>/dev/null || true
    deadline=$(( $(date +%s) + STOP_TIMEOUT_S ))
    while kill -0 "$SPID" 2>/dev/null; do
      if [ "$(date +%s)" -ge "$deadline" ]; then
        kill -KILL "$SPID" 2>/dev/null || true
        sleep 1
        break
      fi
      sleep 0.1 2>/dev/null || sleep 1
    done
  fi
fi
# 3c) SIGINT remaining port listeners (graceful: HTTP drain + final flush).
#     Anything still listening here owns no live pidfile (started by hand, or
#     already SIGKILLed above and lingering).
PIDS=$(lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)
if [ -n "$PIDS" ]; then
  kill -INT $PIDS 2>/dev/null || true
fi
# 3d) wait for the port to be released; escalate to SIGKILL on stragglers.
#     Wall-clock budget (date +%s), not an iteration count: the sleep below
#     falls back to `sleep 1` on /bin/sh without fractional sleep, which would
#     silently multiply any iteration-based budget by 10.
if ! wait_port_down; then
  PIDS=$(lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)
  if [ -n "$PIDS" ]; then
    kill -KILL $PIDS 2>/dev/null || true
    sleep 1
  fi
fi
if port_listening; then
  echo "timeout: port $PORT still listening ${STOP_TIMEOUT_S}s after stop signals; aborting before swap-back" >&2
  # The running process is still the OLD binary (swapped-out inode); put the
  # old binary back on disk so disk matches what is serving.
  if [ -f model-proxy.old ]; then
    mv model-proxy.old model-proxy
  fi
  exit 1
fi

# 4) start the daemon and gate on the port coming back — same invocation, no
#    gap for a second tool call to creep into. daemonize detaches the
#    supervisor itself (setsid, stdio → log file); it refuses when a live
#    daemon already holds the pidfile.
start_daemon() {
  ./model-proxy serve daemon --log-file "$LOG"
}

# stop_everything_best_effort: rollback prologue — a daemon left holding the
# pidfile (or the port) would make the rollback daemonize refuse or the
# worker fail to bind.
stop_everything_best_effort() {
  ./model-proxy serve stop --log-file "$LOG" >/dev/null 2>&1 || true
  ./model-proxy serve stop >/dev/null 2>&1 || true
  PIDS=$(lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)
  if [ -n "$PIDS" ]; then
    kill -INT $PIDS 2>/dev/null || true
  fi
  if ! wait_port_down; then
    PIDS=$(lsof -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null || true)
    if [ -n "$PIDS" ]; then
      kill -KILL $PIDS 2>/dev/null || true
      sleep 1
    fi
  fi
}

if start_daemon && wait_port_up; then
  echo "model-proxy serve daemon restarted on port $PORT (log: $LOG, pidfile: ${LOG%.log}.pid)"
  exit 0
fi

# 5) new binary failed to serve — roll back to the previous binary and bring
#    the daemon back up on it. The gateway must not stay down because a fresh
#    build could not bind.
echo "warning: new binary failed to serve on port $PORT — rolling back to model-proxy.old" >&2
stop_everything_best_effort
if [ -f model-proxy.old ]; then
  mv model-proxy.old model-proxy
  if start_daemon && wait_port_up; then
    echo "rolled back: previous binary serving on port $PORT (log: $LOG); deployment FAILED, gateway is on the old binary" >&2
    exit 1
  fi
  echo "ERROR: rollback start failed too — port $PORT is DOWN; old binary restored on disk, inspect $LOG" >&2
  exit 1
fi
echo "ERROR: start failed and no rollback snapshot exists (model-proxy.old; --build creates it) — port $PORT is DOWN, inspect $LOG" >&2
exit 1
