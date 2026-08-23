#!/usr/bin/env bash
# cover.sh — generate statement coverage report + HTML, list the lowest-covered
# functions (Go has no native branch coverage, so we surface low-coverage
# functions whose branches are likely under-tested), and enforce an 80%
# baseline. Historical gaps have explicit non-regressing package floors.
#
# Usage:
#   scripts/cover.sh                 # default: 80% baseline + 60% function list
#   scripts/cover.sh 70              # custom function-list threshold (baseline stays 80)
#   scripts/cover.sh 70 --no-enforce # skip the baseline gate
#   scripts/cover.sh --self-test     # parser + floor negative-contract checks
#
# Output:
#   cov.out            — raw coverage profile
#   coverage.html      — per-line HTML report (open in a browser)
#   stdout             — per-package statement coverage + lowest-covered functions
set -euo pipefail
cd "$(dirname "$0")/.."

threshold=60
enforce=1
self_test=0
threshold_set=0
for a in "$@"; do
  case "$a" in
    --no-enforce) enforce=0 ;;
    --self-test) self_test=1 ;;
    -*) echo "unknown option: $a" >&2; exit 2 ;;
    *)
      if [ "$threshold_set" -eq 1 ]; then
        echo "unexpected argument: $a" >&2
        exit 2
      fi
      threshold="$a"
      threshold_set=1
      ;;
  esac
done
if ! [[ "$threshold" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "function threshold must be a non-negative number: $threshold" >&2
  exit 2
fi
baseline=80

# Historical packages below the repository-wide baseline have an explicit
# floor. Unlike a blanket exemption, this makes any regression fail while the
# remaining gap stays visible. Raise a floor whenever durable tests improve it.
historical_floor_packages=" model-proxy/internal/cli model-proxy/internal/cli/framework model-proxy/internal/cli/login model-proxy/internal/cli/models model-proxy/internal/cli/serve model-proxy/internal/httpx model-proxy/internal/targetexec model-proxy/scripts/soak "
coverage_floor_for() {
  case "$1" in
    model-proxy/internal/cli) echo "78.8" ;;
    model-proxy/internal/cli/framework) echo "74.6" ;;
    model-proxy/internal/cli/login) echo "62.7" ;;
    model-proxy/internal/cli/models) echo "61.8" ;;
    model-proxy/internal/cli/serve) echo "29.7" ;;
    model-proxy/internal/httpx) echo "75.0" ;;
    model-proxy/internal/targetexec) echo "79.1" ;;
    # CLI harness: main/flag parsing are interactive-only; the scenario
    # builders and the run loop are covered by main_test.go. Floor re-measured
    # under go1.27 (statement-counting drift vs the go1.26-era 56.7).
    model-proxy/scripts/soak) echo "54.1" ;;
    *) echo "$baseline" ;;
  esac
}

coverage_percent_from_line() {
  awk '
    match($0, /coverage: [0-9.]+%/) {
      value = substr($0, RSTART + 10, RLENGTH - 11)
      print value
    }
  ' <<<"$1"
}

coverage_meets_floor() {
  local pkg="$1" pct="$2" floor
  floor="$(coverage_floor_for "$pkg")"
  awk -v pct="$pct" -v floor="$floor" 'BEGIN { exit !(pct + 0 >= floor + 0) }'
}

run_self_test() {
  local parsed pkg floor below_floor
  parsed="$(coverage_percent_from_line $'ok  \tmodel-proxy/internal/example\t0.1s\tcoverage: 81.25% of statements')"
  [ "$parsed" = "81.25" ] || {
    echo "FAIL  coverage parser returned '$parsed', want 81.25" >&2
    return 1
  }
  parsed="$(coverage_percent_from_line $'ok  \tmodel-proxy/internal/archtest\tcoverage: [no statements]')"
  [ -z "$parsed" ] || {
    echo "FAIL  no-statements parser returned '$parsed', want empty" >&2
    return 1
  }
  for pkg in $historical_floor_packages; do
    floor="$(coverage_floor_for "$pkg")"
    if coverage_meets_floor "$pkg" 0; then
      echo "FAIL  $pkg accepted 0% against floor ${floor}%" >&2
      return 1
    fi
    coverage_meets_floor "$pkg" "$floor" || {
      echo "FAIL  $pkg rejected its exact floor ${floor}%" >&2
      return 1
    }
    below_floor="$(awk -v floor="$floor" 'BEGIN { printf "%.1f", floor - 0.1 }')"
    if coverage_meets_floor "$pkg" "$below_floor"; then
      echo "FAIL  $pkg accepted ${below_floor}% below floor ${floor}%" >&2
      return 1
    fi
  done
  if coverage_meets_floor model-proxy/internal/example 79.9; then
    echo "FAIL  ordinary package accepted 79.9% against 80% baseline" >&2
    return 1
  fi
  coverage_meets_floor model-proxy/internal/example 80 || {
    echo "FAIL  ordinary package rejected exact 80% baseline" >&2
    return 1
  }
  echo "✓ coverage parser and floor negative contracts passed."
}

if [ "$self_test" -eq 1 ]; then
  run_self_test
  exit $?
fi

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
  # Packages with production statements but no test files emit a bare 0.0%
  # coverage line. They fail by default; only composition-only packages listed
  # here may remain deliberately testless.
  no_test_exemptions=" model-proxy "
  expected=$(go list ./... | wc -l | tr -d ' ')
  # With -coverprofile, packages without test files print a bare
  # "\tpkg\t\tcoverage: 0.0% of statements" line (no ok/? prefix). These
  # lines are counted below and fail unless explicitly allowlisted.
  # Test-only packages print "ok ... coverage: [no statements]" and remain
  # valid. Any other missing package output still fails the gate.
  seen=$(awk '/^ok[[:space:]]/ {n++} END {print n+0}' "$test_output")
  notests=$(awk '/^\t[^\t]+\t\tcoverage: / {n++} END {print n+0}' "$test_output")
  if [ "$((seen + notests))" -ne "$expected" ]; then
    echo "FAIL  coverage output contains $((seen + notests))/${expected} expected packages"
    fail=1
  fi
  while IFS= read -r line; do
    # line like: "ok  	model-proxy	30.0s	coverage: 80.2% of statements"
    pct=$(coverage_percent_from_line "$line")
    pkg=$(echo "$line" | awk '{print $2}')
    if [ -z "$pct" ]; then
      if echo "$line" | grep -q 'coverage: \[no statements\]'; then
        echo "ok    $pkg  (no statements)"
        continue
      fi
      echo "FAIL  $pkg  missing coverage percentage"
      fail=1
    elif ! coverage_meets_floor "$pkg" "$pct"; then
      floor=$(coverage_floor_for "$pkg")
      echo "FAIL  $pkg  ${pct}% < ${floor}% floor"
      fail=1
    elif awk -v pct="$pct" -v baseline="$baseline" 'BEGIN { exit !(pct + 0 < baseline + 0) }'; then
      floor=$(coverage_floor_for "$pkg")
      echo "gap   $pkg  ${pct}% >= ${floor}% floor, < ${baseline}% baseline"
    else
      echo "ok    $pkg  ${pct}%"
    fi
  done < <(grep -E '^ok[[:space:]]' "$test_output")
  while IFS= read -r line; do
    # line like: "\tmodel-proxy\t\tcoverage: 0.0% of statements"
    pkg=$(echo "$line" | awk '{print $1}')
    pct=$(coverage_percent_from_line "$line")
    if echo "$line" | grep -q 'coverage: \[no statements\]'; then
      echo "ok    $pkg  (no statements)"
    elif [ -z "$pct" ]; then
      echo "FAIL  $pkg  missing coverage percentage"
      fail=1
    elif [ "${no_test_exemptions#* $pkg }" != "$no_test_exemptions" ]; then
      echo "ok    $pkg  ${pct}% (explicit no-test exemption)"
    else
      echo "FAIL  $pkg  ${pct}% (production statements, no test files)"
      fail=1
    fi
  done < <(awk '/^\t[^\t]+\t\tcoverage: / {print}' "$test_output")
  if [ "$fail" -eq 1 ]; then
    echo ""
    echo "✗ Coverage floor gate failed. Raise coverage; historical floors may never decline."
    exit 1
  fi
  echo ""
  echo "✓ Coverage gate passed (${baseline}% baseline plus non-regressing historical floors)."
fi
