#!/usr/bin/env bash
# Report candidates for periodic code review. Analysis errors fail the run.
set -euo pipefail
cd "$(dirname "$0")/.."

# Run native tools even when their package loaders target another platform.
host_os=$(go env GOHOSTOS)
host_arch=$(go env GOHOSTARCH)
host_tool() {
	GOOS="$host_os" GOARCH="$host_arch" go tool -n "$1"
}
unparam=$(host_tool unparam)
deadcode=$(host_tool deadcode)
gocognit=$(host_tool gocognit)
goda=$(host_tool goda)

echo '==> unparam (production packages)'
# unparam exits 1 for findings on stdout and also for errors on stderr.
# On analysis failure it exits before printing any findings.
findings=$("$unparam" ./...) || {
	status=$?
	if [[ $status != 1 || -z $findings ]]; then
		exit "$status"
	fi
}
if [[ -n $findings ]]; then
	printf '%s\n' "$findings"
fi

echo '==> deadcode (executable entry points)'
"$deadcode" ./...

echo '==> deadcode (including test entry points)'
"$deadcode" -test ./...

echo '==> gocognit (10 most complex production functions)'
"$gocognit" -exact-recursion -test=false -top 10 .

echo '==> goda (each internal package followed by its direct internal imports)'
"$goda" graph -type digraph './internal/...'
