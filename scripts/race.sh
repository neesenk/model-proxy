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
#   scripts/race.sh                 # race the default concurrency-heavy set
#   scripts/race.sh ./internal/foo  # race specific packages instead
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

pkgs=("$@")
if [ ${#pkgs[@]} -eq 0 ]; then
	pkgs=("${DEFAULT_PKGS[@]}")
fi

echo "go test -race -count=1 ${pkgs[*]}"
exec go test -race -count=1 "${pkgs[@]}"
