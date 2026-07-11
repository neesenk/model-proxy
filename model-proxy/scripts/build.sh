#!/usr/bin/env bash
# build.sh - compile model-proxy for one or more targets.
#
# Targets are GOOS/GOARCH pairs (the modernc.org/sqlite driver is pure Go, so
# every build is CGO-free and fully static - no cross-toolchain needed).
#
# Usage:
#   scripts/build.sh                    # build the host target only
#   scripts/build.sh linux/amd64        # one target
#   scripts/build.sh linux/amd64 darwin/arm64 linux/arm64   # several
#   scripts/build.sh all                # the common matrix (see DEFAULT_TARGETS)
#   scripts/build.sh host               # alias for the host target
#
# Each target writes dist/model-proxy-<goos>-<goarch> (or model-proxy.exe on
# windows). The version is stamped from git (tag -> describe -> short sha, with
# a -dirty suffix) via -ldflags, overriding the "dev" default in version.go.
#
# Flags:
#   --version <ver>   override the git-derived version string
#   --out <dir>       output directory (default: dist), created if missing
#   --strip           pass -s -w to drop debug info / symbol table (~30% smaller)
#   -v, --verbose     print the full go build command for each target
#   -h, --help        print this help
#
# Examples:
#   scripts/build.sh --strip linux/amd64
#   scripts/build.sh all --out release
#   scripts/build.sh --version v1.2.3 darwin/arm64
set -euo pipefail
cd "$(dirname "$0")/.."

# The module root is this directory (go.mod lives here, not at the repo root).
outdir="dist"
version_override=""
strip=0
verbose=0
targets=()

usage() {
	sed -n '2,/^set -euo pipefail$/p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

# Parse flags first, then positional targets.
while [ $# -gt 0 ]; do
	case "$1" in
		-h|--help) usage 0 ;;
		-v|--verbose) verbose=1; shift ;;
		--strip) strip=1; shift ;;
		--version)
			[ $# -ge 2 ] || { echo "--version needs a value" >&2; exit 2; }
			version_override="$2"; shift 2 ;;
		--version=*) version_override="${1#--version=}"; shift ;;
		--out)
			[ $# -ge 2 ] || { echo "--out needs a value" >&2; exit 2; }
			outdir="$2"; shift 2 ;;
		--out=*) outdir="${1#--out=}"; shift ;;
		--) shift; break ;;
		-*) echo "unknown flag: $1" >&2; exit 2 ;;
		*) targets+=("$1"); shift ;;
	esac
done

# Default target matrix for `all`. Kept to the platforms model-proxy actually
# runs on; add pairs here as needed.
DEFAULT_TARGETS=(
	linux/amd64
	linux/arm64
	darwin/amd64
	darwin/arm64
	windows/amd64
)

# Resolve the version string from git unless overridden. git describe prefers a
# tag, falls back to the short sha, and appends -dirty on uncommitted changes.
resolve_version() {
	if [ -n "$version_override" ]; then
		echo "$version_override"
		return
	fi
	if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
		echo "dev"
		return
	fi
	# --tags so lightweight tags count; --always so a tagless repo still yields
	# the short sha instead of failing.
	git describe --tags --always --dirty 2>/dev/null || echo "dev"
}

version="$(resolve_version)"

# Translate `all` / `host` into concrete GOOS/GOARCH lists.
expanded=()
for t in "${targets[@]:-}"; do
	[ -z "$t" ] && continue
	case "$t" in
		all) expanded+=("${DEFAULT_TARGETS[@]}") ;;
		host) expanded+=("$(go env GOOS)/$(go env GOARCH)") ;;
		*) expanded+=("$t") ;;
	esac
done
# No targets -> host only (mirrors the plain `go build` default).
if [ ${#expanded[@]} -eq 0 ]; then
	expanded=("$(go env GOOS)/$(go env GOARCH)")
fi

mkdir -p "$outdir"

ldflags="-X main.version=${version}"
if [ "$strip" -eq 1 ]; then
	ldflags="${ldflags} -s -w"
fi

build_one() {
	local target="$1"
	local goos goarch
	goos="${target%%/*}"
	goarch="${target#*/}"
	if [ -z "$goos" ] || [ -z "$goarch" ] || [ "$goos" = "$goarch" ]; then
		echo "✗ bad target '$target' (want goos/goarch, e.g. linux/amd64)" >&2
		return 1
	fi

	# windows gets .exe so the binary is double-clickable / recognizable there.
	local out
	if [ "$goos" = "windows" ]; then
		out="${outdir}/model-proxy-${goos}-${goarch}.exe"
	else
		out="${outdir}/model-proxy-${goos}-${goarch}"
	fi

	local cmd=(env GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0
		go build -trimpath -ldflags "$ldflags" -o "$out" .)
	if [ "$verbose" -eq 1 ]; then
		# shellcheck disable=SC2145
		echo "$ ${cmd[@]}"
	fi

	if "${cmd[@]}"; then
		# `file` may be absent on minimal systems; size is always available.
		local info
		if info=$(file "$out" 2>/dev/null); then
			info=${info#*: } # drop the leading "path: "
		else
			info="$(du -h "$out" | cut -f1)"
		fi
		printf '%-18s -> %s  (%s, %s)\n' "$goos/$goarch" "$out" "$info" "v$version"
	else
		echo "✗ build failed: $goos/$goarch" >&2
		return 1
	fi
}

echo "model-proxy build: version=${version} out=${outdir}/"
fail=0
# `set -e` is on, so a failing build_one would abort the loop; track failures
# instead so one bad target doesn't hide the rest.
for t in "${expanded[@]}"; do
	if ! build_one "$t"; then
		fail=1
	fi
done

echo ""
if [ "$fail" -eq 1 ]; then
	echo "✗ one or more targets failed."
	exit 1
fi
echo "✓ built ${#expanded[@]} target(s) into ${outdir}/"
