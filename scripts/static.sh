#!/usr/bin/env bash
# static.sh - static analysis gate: staticcheck + errcheck + dupl + govulncheck.
#
# The deterministic static counterpart to `go vet`: staticcheck catches bug
# patterns, dead code and duplicated imports vet does not (SA*, S*, ST1019,
# U1000); errcheck fails on unchecked error returns in production code (test
# stubs are excluded via -ignoretests: their writes/decodes are self-checking
# through later assertions); dupl ratchets copy/paste duplication down (AI's
# signature smell — the count must never exceed scripts/dupl-baseline.txt);
# govulncheck fails the build on vulnerabilities REACHABLE from this
# repository's own code (symbol-level scan, the right strictness for an
# application, not a library). All tools are pinned by the `tool` directives
# in go.mod and run via `go tool`, so every machine uses the same version and
# no global install is needed (downloaded with the module graph like any
# dependency). govulncheck fetches the vuln DB from vuln.go.dev on each run.
#
# Usage:
#   scripts/static.sh    # full gate over ./...
#
# Exit code is non-zero if any tool reports a finding.
#
# Suppression policy (mirrors the test contract: no blanket waivers):
# - staticcheck: a finding is fixed, or suppressed at the exact site with
#   `//lint:ignore <CheckID> <reason>`;
# - errcheck: intentional ignores are explicit `_ =` / `_, _ =` at the call
#   site (drain copies, best-effort cleanup removes, fire-and-forget
#   notifications); only the conventionally-unactionable symbol set in
#   scripts/errcheck-excludes.txt is excluded — extend it rarely, with a
#   rationale comment per symbol;
# - dupl: no per-site suppression; a deliberate clone is accepted by raising
#   the baseline WITH a justification in the same commit;
# - U1000 means delete the dead code, not silence the check;
# - version bumps (go get -tool <path>@<version>) that add checks land
#   together with the fixes.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> staticcheck $(go list -m -f '{{.Version}}' honnef.co/go/tools)"
go tool staticcheck ./...

echo "==> errcheck $(go list -m -f '{{.Version}}' github.com/kisielk/errcheck)"
go tool errcheck -exclude scripts/errcheck-excludes.txt -ignoretests ./...

echo "==> dupl $(go list -m -f '{{.Version}}' github.com/mibk/dupl)"
baseline=$(grep -E '^[0-9]+$' scripts/dupl-baseline.txt)
groups=$(go tool dupl -threshold 120 ./internal ./scripts 2>/dev/null | grep -c '^found' || true)
if [ "$groups" -gt "$baseline" ]; then
	echo "dupl: $groups clone groups exceed baseline $baseline (see scripts/dupl-baseline.txt);" >&2
	echo "       dedup the new clones or, if deliberate, raise the baseline with justification." >&2
	exit 1
fi
echo "    dupl: $groups/$baseline clone groups"

echo "==> govulncheck $(go list -m -f '{{.Version}}' golang.org/x/vuln)"
go tool govulncheck ./...
