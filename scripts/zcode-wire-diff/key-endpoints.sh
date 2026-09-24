#!/usr/bin/env bash
# zcode-wire-diff/key-endpoints.sh — pinpoint which BigModel surface a key is
# valid on. A Coding Plan key is not necessarily accepted by every endpoint
# family: open.bigmodel.cn serves the general API under /api/paas/v4, the
# coding-plan API under /api/coding/paas/v4 (both exist; verified 2026-09-24),
# the Anthropic surface under /api/anthropic, and ZCode's platform gateway
# under zcode.z.ai/api/v1/ultra/anthropic. A key rejected everywhere is simply
# invalid; a key accepted on exactly one family tells us which base URL the
# provider must use.
#
# Usage:
#   scripts/zcode-wire-diff/key-endpoints.sh <API_KEY>
#   scripts/zcode-wire-diff/key-endpoints.sh          # reads the key from the
#                                                     # macOS clipboard (pbpaste)
# Nothing is written anywhere; the key is only used in request headers.
set -euo pipefail

KEY="${1:-}"
if [ -z "$KEY" ]; then
  command -v pbpaste >/dev/null || { echo "no key given and pbpaste unavailable" >&2; exit 1; }
  KEY="$(pbpaste)"
fi
[ -n "$KEY" ] || { echo "empty key" >&2; exit 1; }

echo "key: length=${#KEY} prefix=${KEY:0:3}… suffix=${KEY: -4}"
echo

BODY='{"model":"glm-5.3","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}'

get() {
  local label="$1" url="$2"
  printf '%-46s ' "$label"
  curl -sS --noproxy '*' --max-time 25 -o /tmp/zcode-ep.json -w 'HTTP %{http_code}  ' \
    -H "Authorization: Bearer $KEY" "$url" 2>/dev/null || echo -n "curl-fail  "
  head -c 150 /tmp/zcode-ep.json 2>/dev/null; echo
}

post() {
  local label="$1" url="$2"; shift 2
  printf '%-46s ' "$label"
  curl -sS --noproxy '*' --max-time 25 -o /tmp/zcode-ep.json -w 'HTTP %{http_code}  ' \
    -X POST -H "Authorization: Bearer $KEY" -H "x-api-key: $KEY" \
    -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
    -H "user-agent: ZCode/3.14.0 ai-sdk/provider-utils/4.0.27 runtime/node.js/24" \
    -H "http-referer: https://zcode.z.ai" -H "x-title: Z Code@cli" \
    -H "x-zcode-app-version: 3.14.0" -H "x-zcode-agent: glm" \
    -H "x-zcode-session-type: main" -H "x-release-channel: production" \
    --data-raw "$BODY" "$url" 2>/dev/null || echo -n "curl-fail  "
  head -c 150 /tmp/zcode-ep.json 2>/dev/null; echo
}

echo "=== GET endpoints (key liveness per family) ==="
get "general   /api/paas/v4/models"        "https://open.bigmodel.cn/api/paas/v4/models"
get "coding    /api/coding/paas/v4/models" "https://open.bigmodel.cn/api/coding/paas/v4/models"
get "quota     /api/monitor/usage/quota"   "https://open.bigmodel.cn/api/monitor/usage/quota/limit"
echo
echo "=== POST endpoints (model call, full ZCode fingerprint) ==="
post "general   /api/anthropic/v1/messages"      "https://open.bigmodel.cn/api/anthropic/v1/messages"
post "gateway   zcode.z.ai/ultra/anthropic"      "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages"
post "zai       api.z.ai/api/anthropic"          "https://api.z.ai/api/anthropic/v1/messages"
echo
cat <<'EOF'
=== how to read this ===
* NOTHING works (all 401)                  -> the key itself is not a usable
  BigModel key (truncated copy, masked value, or revoked). Nothing model-proxy
  can do; re-copy it in full or create a new one.
* /api/coding/paas/v4 works, others 401     -> coding-plan-scoped key. The
  Anthropic surface for it does not exist (/api/coding/anthropic 404s), so the
  provider must be reconfigured (report the output).
* /api/anthropic works                       -> key is fine on the path this
  provider targets; a login failure would then be a model-proxy bug.
* Only the zcode.z.ai gateway works          -> switch anthropic_base_url to
  https://zcode.z.ai/api/v1/ultra/anthropic.
EOF
