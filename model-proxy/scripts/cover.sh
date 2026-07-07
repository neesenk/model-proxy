#!/usr/bin/env bash
# cover.sh — generate statement coverage report + HTML, and list the
# lowest-covered functions (branch-coverage proxy: Go has no native branch
# coverage, so we surface low-coverage functions whose branches are likely
# under-tested).
#
# Usage:
#   scripts/cover.sh                 # default: 60% function threshold
#   scripts/cover.sh 70              # custom threshold for the "low coverage" list
#
# Output:
#   cov.out            — raw coverage profile
#   coverage.html      — per-line HTML report (open in a browser)
#   stdout             — per-package statement coverage + lowest-covered functions
set -euo pipefail
cd "$(dirname "$0")/.."

threshold="${1:-60}"

echo "=== Building & running all tests with coverage ==="
go test -coverprofile=cov.out -covermode=count ./... 2>&1 | grep -E "^(ok|FAIL|---)" || true

echo ""
echo "=== Statement coverage by package ==="
go tool cover -func=cov.out | awk '/^total:/ {print; next} /\t0\.0%$/ {next} {print}' | tail -1
echo ""
go test -cover ./... 2>&1 | grep -E "^(ok|FAIL)" || true

echo ""
echo "=== Functions below ${threshold}% coverage (branch-coverage proxy) ==="
go tool cover -func=cov.out | awk -v t="$threshold" '
  /^total:/ {next}
  {
    # last field is like "42.9%"
    pct=$NF; gsub(/%/,"",pct);
    if (pct+0 < t) print pct"%\t" $0
  }' | sort -n | head -30

echo ""
echo "=== HTML report ==="
go tool cover -html=cov.out -o coverage.html
echo "wrote coverage.html ($(du -h coverage.html | cut -f1)) — open it: file://$PWD/coverage.html"
echo ""
echo "wrote cov.out ($(du -h cov.out | cut -f1))"
