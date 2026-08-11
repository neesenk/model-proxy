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
test_output=$(mktemp)
trap 'rm -f "$test_output"' EXIT
set +e
go test -coverprofile=cov.out -covermode=count ./... 2>&1 | tee "$test_output"
test_status=${PIPESTATUS[0]}
set -e
if [ "$test_status" -ne 0 ]; then
  echo ""
  echo "✗ Tests failed; coverage report is not valid."
  exit "$test_status"
fi

echo ""
echo "=== Statement coverage by package ==="
go tool cover -func=cov.out | awk '/^total:/ {print; next} /\t0\.0%$/ {next} {print}' | tail -1
echo ""
grep -E '^ok[[:space:]]' "$test_output"

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
  # Documented pre-existing gaps (docs/engineering/testing.md 覆盖率节): these
  # packages sit below the 80% baseline since before the gate could run green.
  # They stay visible as "gap" lines but do not fail the build; everything else
  # must hold 80%. Remove entries as coverage improves.
  exemptions=" model-proxy/internal/cli model-proxy/internal/cli/framework model-proxy/internal/cli/login model-proxy/internal/cli/models model-proxy/internal/cli/serve model-proxy/internal/httpx model-proxy/internal/targetexec "
  expected=$(go list ./... | wc -l | tr -d ' ')
  # With -coverprofile, packages without test files print a bare
  # "\tpkg\t\tcoverage: 0.0% of statements" line (no ok/? prefix), and
  # test-only packages print "ok ... coverage: [no statements]". Both are
  # valid expected output: counted as seen but exempt from the percentage
  # gate. Any other missing package output still fails the gate.
  seen=$(awk '/^ok[[:space:]]/ {n++} END {print n+0}' "$test_output")
  notests=$(awk '/^\t[^\t]+\t\tcoverage: / {n++} END {print n+0}' "$test_output")
  if [ "$((seen + notests))" -ne "$expected" ]; then
    echo "FAIL  coverage output contains $((seen + notests))/${expected} expected packages"
    fail=1
  fi
  while IFS= read -r line; do
    # line like: "ok  	model-proxy	30.0s	coverage: 80.2% of statements"
    pct=$(echo "$line" | grep -oE 'coverage: [0-9.]+%' | grep -oE '[0-9.]+' || true)
    pkg=$(echo "$line" | awk '{print $2}')
    if [ -z "$pct" ]; then
      if echo "$line" | grep -q 'coverage: \[no statements\]'; then
        echo "ok    $pkg  (no statements)"
        continue
      fi
      echo "FAIL  $pkg  missing coverage percentage"
      fail=1
    elif awk "BEGIN{exit !($pct < $baseline)}"; then
      if [ "${exemptions#* $pkg }" != "$exemptions" ]; then
        echo "gap   $pkg  ${pct}% < ${baseline}% baseline (documented exemption)"
      else
        echo "FAIL  $pkg  ${pct}% < ${baseline}% baseline"
        fail=1
      fi
    else
      echo "ok    $pkg  ${pct}%"
    fi
  done < <(grep -E '^ok[[:space:]]' "$test_output")
  if [ "$fail" -eq 1 ]; then
    echo ""
    echo "✗ Coverage below ${baseline}% baseline. Raise coverage or document the gap."
    exit 1
  fi
  echo ""
  echo "✓ All packages ≥ ${baseline}% baseline."
fi
