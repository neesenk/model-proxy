#!/usr/bin/env bash
# cover.sh — generate statement coverage report + HTML, list the lowest-covered
# functions (Go has no native branch coverage, so we surface low-coverage
# functions whose branches are likely under-tested), and enforce an 80%
# baseline. Exits non-zero if any package drops below the baseline.
#
# Usage:
#   scripts/cover.sh                 # default: 80% baseline + 60% function list
#   scripts/cover.sh 70              # custom function-list threshold (baseline stays 80)
#   scripts/cover.sh 70 --no-enforce # skip the baseline gate
#
# Output:
#   cov.out            — raw coverage profile
#   coverage.html      — per-line HTML report (open in a browser)
#   stdout             — per-package statement coverage + lowest-covered functions
set -euo pipefail
cd "$(dirname "$0")/.."

threshold="${1:-60}"
enforce=1
for a in "$@"; do [ "$a" = "--no-enforce" ] && enforce=0; done
baseline=80

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

# --- 80% baseline enforcement ---
if [ "$enforce" -eq 1 ]; then
  echo ""
  echo "=== Baseline gate (${baseline}% per package) ==="
  fail=0
  while IFS= read -r line; do
    # line like: "ok  	model-proxy	30.0s	coverage: 80.2% of statements"
    pct=$(echo "$line" | grep -oE 'coverage: [0-9.]+%' | grep -oE '[0-9.]+')
    pkg=$(echo "$line" | awk '{print $2}')
    if [ -n "$pct" ] && awk "BEGIN{exit !($pct < $baseline)}"; then
      echo "FAIL  $pkg  ${pct}% < ${baseline}% baseline"
      fail=1
    else
      echo "ok    $pkg  ${pct}%"
    fi
  done < <(go test -cover ./... 2>&1 | grep -E '^ok')
  if [ "$fail" -eq 1 ]; then
    echo ""
    echo "✗ Coverage below ${baseline}% baseline. Raise coverage or document the gap."
    exit 1
  fi
  echo ""
  echo "✓ All packages ≥ ${baseline}% baseline."
fi
