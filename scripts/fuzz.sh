#!/usr/bin/env bash
# fuzz.sh — run every native fuzz target with a per-target time budget.
#
# Seeds run as normal tests in `go test ./...` (free regression). Real
# exploration is this manual, periodic gate: each target runs -fuzz with a
# budget (default 15s; pass a number for more, e.g. `scripts/fuzz.sh 120`
# before a release). A crasher lands in testdata/fuzz/<Target>/ together
# with a failing-input message — minimize, fix, and commit the corpus file
# so the seed matrix pins the bug forever.
#
# A NEW fuzz target must be registered here AND in
# docs/engineering/testing.md (§ Fuzz target list) in the same commit.
set -euo pipefail
cd "$(dirname "$0")/.."

per_target="${1:-15}"

run() {
	local pkg="$1" target="$2"
	echo "==> ${target} (${pkg}) — ${per_target}s"
	go test -run '^$' -fuzz "^${target}\$" -fuzztime "${per_target}s" "$pkg"
}

# Protocol conversion (original quartet).
run ./internal/protocol FuzzConvertRequest
run ./internal/protocol FuzzConvertSSE
run ./internal/protocol FuzzParseToolArgs
run ./internal/protocol FuzzSanitizeToolUseID
# Wire SSE parsing and the sonic fast paths (differential vs encoding/json).
run ./internal/protocol FuzzParseWireSSE
run ./internal/protocol FuzzExtractModelDifferential
run ./internal/protocol FuzzRequestWantsStreamDifferential
# Guard secret scanning: detection, redaction, credential red line.
run ./internal/guard FuzzScanSecretsAndPaths
# Quota window parsing (structural contract).
run ./internal/provider FuzzParseVolcengineQuota
# Takeover config editor (hostile-YAML round-trip).
run ./internal/takeover FuzzYAMLSetGetRoundTrip
