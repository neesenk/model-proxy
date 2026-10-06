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
#   cov.out            — raw coverage profile (count mode)
#   coverage.html      — per-line HTML report (open in a browser)
#   stdout             — per-package statement coverage + lowest-covered
#                        functions + single-path block list (weak-branch signal)
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

# Coverage statement counting is toolchain-sensitive (docs/engineering/
# pitfalls.md #34): CI pins the toolchain via go-version-file: go.mod, so a
# different local toolchain can shift per-package numbers by a few points.
# Warn (never fail) on a mismatch — the CI measurement stays authoritative.
gomod_go=$(awk '/^go /{print $2; exit}' go.mod)
local_go=$(go version | awk '{print $3}' | sed 's/^go//')
if [ -n "$gomod_go" ] && [ "$local_go" != "$gomod_go" ]; then
  echo "⚠ cover.sh: toolchain mismatch (local go$local_go, go.mod go$gomod_go) — coverage numbers may drift from CI; the CI measurement is authoritative" >&2
fi

# Historical packages below the repository-wide baseline have an explicit
# floor. Unlike a blanket exemption, this makes any regression fail while the
# remaining gap stays visible. Raise a floor whenever durable tests improve it.
historical_floor_packages=" model-proxy model-proxy/internal/cli model-proxy/internal/cli/framework model-proxy/internal/cli/login model-proxy/internal/cli/models model-proxy/internal/cli/serve model-proxy/internal/httpx model-proxy/internal/targetexec model-proxy/scripts/soak model-proxy/scripts/e2eguard "
coverage_floor_for() {
  case "$1" in
    # Root package main is process-entry/composition-only (signal/listener/
    # drain orchestration is interactive-only); it previously held the explicit
    # no-test exemption. main_test.go pins the build-version wiring; the floor
    # keeps that non-regressing. Measured 2.6% under go1.27.
    model-proxy) echo "2.5" ;;
    model-proxy/internal/cli) echo "78.8" ;;
    model-proxy/internal/cli/framework) echo "74.6" ;;
    model-proxy/internal/cli/login) echo "62.7" ;;
    model-proxy/internal/cli/models) echo "67.2" ;;
    model-proxy/internal/cli/serve) echo "82.0" ;;
    model-proxy/internal/httpx) echo "75.0" ;;
    model-proxy/internal/targetexec) echo "79.1" ;;
    # CLI harness: main/flag parsing are interactive-only; the scenario
    # builders, the run loop and the pooled client constructor are covered by
    # main_test.go. Floor re-measured under go1.27 (statement-counting drift
    # vs the go1.26-era 56.7).
    model-proxy/scripts/soak) echo "55.0" ;;
    # Same harness shape as soak: main() is an os.Exit wrapper, scenarios
    # drive a live daemon over HTTP. Measured 80.1% under go1.27 locally but
    # 79.3% under the CI-pinned go1.26 (statement-counting drift, pitfalls
    # #34) — the CI measurement is the authoritative floor.
    model-proxy/scripts/e2eguard) echo "79.3" ;;
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

# The profile is count-mode: every covered block carries its execution count.
# A block with count==1 is "covered" by exactly one test path — no independent
# second witness pins its behavior. That is the closest measurable signal Go
# offers to branch coverage (which the toolchain does not provide natively).
# Profile lines: "path/file.go:start.col,end.col numStmts count"; the leading
# "mode:" line has fewer fields and is excluded by the NF guard.
weak_branch_summary() {
  awk '
    NF >= 3 {
      total++
      if ($3 == 1 && $2 > 0) { weak++; stmts += $2 }
    }
    END { printf "%d blocks, %d single-path (%d statements)\n", total, weak, stmts }
  ' "$1"
}

weak_branch_files() {
  awk '
    NF >= 3 && $3 == 1 && $2 > 0 {
      file = $1; sub(/:.*/, "", file)
      blocks[file]++; stmts[file] += $2
    }
    END { for (f in blocks) printf "%3d blocks %5d stmts  %s\n", blocks[f], stmts[f], f }
  ' "$1" | sort -rn | head -15
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
  # weak-branch parsers: mode line excluded, count==0 ignored (that is the
  # coverage gap the package/function numbers already report), count==1 with
  # zero statements ignored (no behavioral weight).
  prof="$(mktemp)"
  printf '%s\n' \
    'mode: count' \
    'm/a.go:1.2,2.10 2 1' \
    'm/a.go:3.2,4.10 1 1' \
    'm/a.go:5.2,6.10 1 5' \
    'm/b.go:7.2,8.10 3 0' \
    'm/c.go:9.2,9.9 0 1' >"$prof"
  summary="$(weak_branch_summary "$prof")"
  files="$(weak_branch_files "$prof")"
  rm -f "$prof"
  [ "$summary" = "5 blocks, 2 single-path (3 statements)" ] || {
    echo "FAIL  weak-branch summary returned '$summary', want '5 blocks, 2 single-path (3 statements)'" >&2
    return 1
  }
  [ "$files" = "  2 blocks     3 stmts  m/a.go" ] || {
    echo "FAIL  weak-branch file list returned '$files', want the single m/a.go line" >&2
    return 1
  }
  echo "✓ coverage parser, floor negative contracts and weak-branch parsers passed."
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
echo "=== Single-path blocks (count==1 — weak-branch signal, not a gate) ==="
echo "covered exactly once: no independent second test pins these branches"
weak_branch_summary cov.out
weak_branch_files cov.out

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
