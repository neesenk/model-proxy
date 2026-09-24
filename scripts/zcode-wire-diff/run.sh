#!/usr/bin/env bash
# zcode-wire-diff/run.sh — capture ONE real model request from the open-sourced
# ZCode CLI (github.com/zai-org/ZCode) and refresh the golden fixture that
# internal/provider/zcode_wire_test.go compares model-proxy's fingerprint
# against. Fully local: the CLI talks to a capture server on 127.0.0.1, never to
# BigModel.
#
# One-time setup (see README.md):
#   git clone https://github.com/zai-org/ZCode.git /tmp/ZCode
#   cd /tmp/ZCode && pnpm install --filter @zcode/cli... && pnpm --filter @zcode/cli... build
#
# Usage:
#   scripts/zcode-wire-diff/run.sh            # capture + show diff vs fixture
#   scripts/zcode-wire-diff/run.sh --write    # capture + overwrite the fixture
#
# Env:
#   ZCODE_SRC   cloned ZCode repo            (default /tmp/ZCode)
set -euo pipefail
cd "$(dirname "$0")/../.."

ZCODE_SRC="${ZCODE_SRC:-/tmp/ZCode}"
OUT=/tmp/zcode-wire-diff
FIXTURE=internal/provider/testdata/zcode-wire/real-zcode-cli.json
CLI_DIST="$ZCODE_SRC/apps/zcode-cli/packages/cli/dist/zcode.cjs"
BUILTIN_CATALOG="$ZCODE_SRC/config/provider/zcode-builtin.json"

write=0
for a in "$@"; do
  case "$a" in
    --write) write=1 ;;
    *) echo "unknown argument: $a (expected --write)" >&2; exit 2 ;;
  esac
done

[ -f "$CLI_DIST" ] || { echo "missing $CLI_DIST — run the one-time setup (README.md)" >&2; exit 1; }
[ -f "$BUILTIN_CATALOG" ] || { echo "missing $BUILTIN_CATALOG" >&2; exit 1; }
command -v node >/dev/null || { echo "node not found" >&2; exit 1; }

rm -rf "$OUT" && mkdir -p "$OUT/captured" "$OUT/home"
CAPTURE_DIR="$OUT/captured" CAPTURE_PORT_FILE="$OUT/port" \
  node scripts/zcode-wire-diff/capture-server.mjs > "$OUT/server.log" 2>&1 &
server_pid=$!
trap 'kill $server_pid 2>/dev/null || true' EXIT

# Wait for the ephemeral port publication (server listens on 0, so a stale
# listener elsewhere can never hijack the capture).
port=""
for _ in $(seq 1 50); do
  [ -s "$OUT/port" ] && { port=$(cat "$OUT/port"); break; }
  sleep 0.1
done
[ -n "$port" ] || { echo "capture server did not start — see $OUT/server.log" >&2; exit 1; }

sed "s|__PORT__|$port|g" scripts/zcode-wire-diff/provider_config.template.json \
  > "$OUT/provider_config.json"

# ZCODE_DATA_BASE_DIR isolates all app state; the two *_CONFIG_FILE env vars pin
# the provider config files (builtin catalog unmodified, personal provider from
# this harness); ZCODE_APP_VERSION pins the fingerprint version under test.
ZCODE_DATA_BASE_DIR="$OUT/home" \
ZCODE_BUILTIN_PROVIDER_CONFIG_FILE="$BUILTIN_CATALOG" \
ZCODE_BUILTIN_PROVIDER_BUNDLED_CONFIG_FILE="$BUILTIN_CATALOG" \
ZCODE_PERSONAL_PROVIDER_CONFIG_FILE="$OUT/provider_config.json" \
ZCODE_APP_VERSION=3.14.0 NO_COLOR=1 \
  node "$CLI_DIST" --prompt "say hi" --output-format json --force \
  > "$OUT/cli-run.log" 2>&1 || true

kill $server_pid 2>/dev/null || true
trap - EXIT

capture=""
for f in "$OUT/captured"/req-*.json; do
  [ -e "$f" ] || continue
  if grep -q '"/v1/messages"' "$f"; then capture="$f"; break; fi
done
[ -n "$capture" ] || { echo "no /v1/messages capture — see $OUT/cli-run.log" >&2; exit 1; }
echo "captured: $capture (port $port)"

node -e '
const fs = require("node:fs");
const cap = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const headers = { ...cap.headers };
headers.authorization = "Bearer <API_KEY>";
headers["x-api-key"] = "<API_KEY>";
const fixture = {
  source: "github.com/zai-org/ZCode v3.14.0 (open-sourced 2026-09-21, Apache-2.0) — real CLI engine run against a local capture server",
  capturedAt: cap.at,
  capturedWith: {
    cli: "node apps/zcode-cli/packages/cli/dist/zcode.cjs --prompt \x27say hi\x27 --output-format json --force",
    node: process.version,
    platform: `${process.platform}-${process.arch}`,
    note: "runtime/node.js/26 reflects the LOCAL node used for the capture; the product pins node 24 (.nvmrc 24.14.0), and model-proxy sends 24 accordingly.",
  },
  method: cap.method,
  path: cap.url,
  headers,
  bodyNotes: "body is the prompt the agent itself sent (system prompt, tools, metadata.user_id with device_id/session_id); model-proxy is a byte-level passthrough and does not touch it.",
  regenerate: "scripts/zcode-wire-diff/run.sh (one-time clone+build; see scripts/zcode-wire-diff/README.md)",
};
fs.writeFileSync(process.argv[2], JSON.stringify(fixture, null, 2) + "\n");
' "$capture" "$OUT/real-zcode-cli.json"

if [ "$write" = "1" ]; then
  cp "$OUT/real-zcode-cli.json" "$FIXTURE"
  echo "fixture updated: $FIXTURE"
else
  echo "--- diff vs checked-in fixture (values only; --write to apply) ---"
  if diff <(python3 -m json.tool "$FIXTURE") <(python3 -m json.tool "$OUT/real-zcode-cli.json"); then
    echo "no differences"
  else
    echo "(differences above — review, then re-run with --write)"
  fi
fi
