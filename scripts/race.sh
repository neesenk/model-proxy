#!/usr/bin/env bash
# race.sh - fast race-detector gate over the concurrency-critical packages.
#
# The full `go test -race ./...` matrix is the authoritative gate (AGENTS.md
# verification section), but it takes minutes - long enough that a change can
# land without anyone actually running it. This script covers the subset where
# data races historically live (per-request snapshotting, reload swaps,
# SSE fan-out, background flushers) in well under a minute, so it fits a
# pre-commit / quick-check loop.
#
# Usage:
#   scripts/race.sh                 # race the default concurrency-heavy set,
#                                   # then the CPU-sensitive subset under
#                                   #   -cpu 1,2 -count=2 (directed repetition)
#   scripts/race.sh ./internal/foo  # first pass only for the given packages
#                                   #   (the CPU matrix still runs)
#
# Exit code is non-zero if any selected package fails or detects a race.
set -euo pipefail
cd "$(dirname "$0")/.."

DEFAULT_PKGS=(
	./internal/app
	./internal/runtime/...
	./internal/targetexec
	./internal/fusion
	./internal/cache
	./internal/shadow
	./internal/guard
	./internal/accounts
	./internal/observe/...
)

# CPU-sensitive subset: the packages where races historically surfaced ONLY
# under constrained scheduling (-cpu 1 turns syscall returns into preemption
# points; it caught the modelcaps persist self-adoption and the forward
# cooldown-window branch flip that default scheduling missed repeatedly).
# docs/engineering/testing.md §并发测试模式目录, pattern P7.
CPU_MATRIX_PKGS=(
	./internal/app
	./internal/runtime/...
	./internal/forward
	./internal/adjudicate
	./internal/accounts
	./internal/observe/stats
	./internal/observe/requestlog
	./internal/mcp
)

pkgs=("$@")
if [ ${#pkgs[@]} -eq 0 ]; then
	pkgs=("${DEFAULT_PKGS[@]}")
fi

echo "go test -race -count=1 ${pkgs[*]}"
go test -race -count=1 "${pkgs[@]}"

# The matrix pass runs the CPU-sensitive subset under -cpu 1,2 with a repeat
# count: interleavings that need constrained scheduling are probabilistic, so
# a single pass is a smoke, two directed repeats is the gate.
echo "go test -race -cpu 1,2 -count=2 ${CPU_MATRIX_PKGS[*]}"
go test -race -cpu 1,2 -count=2 "${CPU_MATRIX_PKGS[@]}"
