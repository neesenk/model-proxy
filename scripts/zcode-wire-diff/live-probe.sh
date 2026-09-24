#!/usr/bin/env bash
# zcode-wire-diff/live-probe.sh — verify (with a REAL Coding Plan key) whether the
# DIRECT BigModel Anthropic path still serves plan traffic for our fingerprint
# simulation. This is the open question left by the 2026-09-21 ZCode open-source
# review: the shipping ZCode 3.14.0 client rewrites
# open.bigmodel.cn/api/anthropic/v1/messages to the zcode.z.ai ultra gateway
# (official-coding-plan-gateway.ts), so the direct path this provider targets is
# no longer what the real client puts on the wire.
#
# Run AFTER `./model-proxy login zcode` (the key is read from the pool file).
# Cost: two 1-token model calls. Never prints the key.
#
# What it answers:
#   1. Does the direct path still accept a Coding Plan key WITH our fingerprint?
#   2. Does the control (same key, NO fingerprint) behave differently — i.e. is
#      the fingerprint still what earns plan treatment?
#   3. Does the quota snapshot move by roughly the plan coefficient?
#
# Usage: scripts/zcode-wire-diff/live-probe.sh [model]   (default glm-5.3)
set -euo pipefail

MODEL="${1:-glm-5.3}"
POOL="${HOME}/.model-proxy/zcode_apikeys.json"
BASE="https://open.bigmodel.cn/api/anthropic/v1/messages"
APPVER="3.14.0"

[ -f "$POOL" ] || { echo "no zcode credential pool at $POOL — run: ./model-proxy login zcode" >&2; exit 1; }
KEY="$(python3 -c "import json;print(json.load(open('$POOL'))['accounts'][0]['api_key'])")"
[ -n "$KEY" ] || { echo "pool has no accounts" >&2; exit 1; }
echo "using zcode account from pool (key masked: ${KEY:0:6}…)"

body=$(python3 -c "import json,sys;print(json.dumps({'model':sys.argv[1],'max_tokens':1,'messages':[{'role':'user','content':'hi'}]}))" "$MODEL")

fire() {
  local label="$1"; shift
  echo "--- $label ---"
  curl -sS -o /tmp/zcode-probe-$label.json -w 'HTTP %{http_code} in %{time_total}s\n' --max-time 60 \
    "$BASE" -H "content-type: application/json" "$@" --data-raw "$body" || echo "curl failed"
  head -c 400 /tmp/zcode-probe-$label.json; echo; echo
}

# 1. Full ZCode fingerprint (what model-proxy sends).
fire with-fingerprint \
  -H "authorization: Bearer $KEY" -H "x-api-key: $KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "user-agent: ZCode/$APPVER ai-sdk/provider-utils/4.0.27 runtime/node.js/24" \
  -H "http-referer: https://zcode.z.ai" \
  -H "x-title: Z Code@cli" -H "x-zcode-app-version: $APPVER" -H "x-zcode-agent: glm" \
  -H "x-zcode-session-type: main" -H "x-release-channel: production" \
  -H "x-platform: $(uname -s | tr 'A-Z' 'a-z')-$(uname -m)" \
  -H "x-request-id: $(python3 -c 'import uuid;print(uuid.uuid4())')" \
  -H "x-session-id: $(python3 -c 'import uuid;print(uuid.uuid4())')"

# 2. Control: same key, plain Anthropic client (no fingerprint).
fire control-no-fingerprint \
  -H "authorization: Bearer $KEY" -H "x-api-key: $KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "user-agent: curl/8"

# 3. Quota snapshot after both calls (plan coefficient shows up here).
echo "--- quota snapshot ---"
./model-proxy usage zcode 2>&1 | head -20 || true
