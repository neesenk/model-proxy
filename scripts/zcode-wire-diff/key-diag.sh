#!/usr/bin/env bash
# zcode-wire-diff/key-diag.sh — diagnose a BigModel Coding Plan key.
#
# `login zcode` validates against ONE endpoint (usage/quota/limit). When it fails
# with 令牌已过期或验证不正确 the key itself is rejected, but the single probe
# cannot say whether the credential is malformed, expired, or fine on another
# product path. This script runs the full battery and prints every response so
# the failure can be attributed precisely.
#
# Usage:   scripts/zcode-wire-diff/key-diag.sh <API_KEY>
# Never writes anything (no pool, no files). The key is only used in request
# headers and is masked in the shape summary.
set -euo pipefail

KEY="${1:?usage: key-diag.sh <API_KEY>}"

echo "=== key shape ==="
has_dot=no
alnum_only=yes
has_asterisk=no
case "$KEY" in *.*) has_dot=yes ;; esac
case "$KEY" in *[!A-Za-z0-9.-]*) alnum_only=no ;; esac
case "$KEY" in *\**) has_asterisk=yes ;; esac
echo "length=${#KEY}  prefix=${KEY:0:3}  suffix=${KEY: -4}  has_dot=$has_dot  alnum_only=$alnum_only  has_asterisk=$has_asterisk"
echo "(BigModel console keys are long; a short value usually means a truncated copy;"
echo " an asterisk means a masked display value was copied, not the key itself)"
echo

probe() {
  local label="$1" url="$2" data="$3"; shift 3
  echo "--- $label ---"
  if [ -n "$data" ]; then
    curl -sS -o /tmp/zcode-keydiag.json -w 'HTTP %{http_code} in %{time_total}s\n' --max-time 30 \
      "$url" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" "$@" --data-raw "$data" || echo "curl failed"
  else
    curl -sS -o /tmp/zcode-keydiag.json -w 'HTTP %{http_code} in %{time_total}s\n' --max-time 30 \
      "$url" -H "Authorization: Bearer $KEY" "$@" || echo "curl failed"
  fi
  head -c 300 /tmp/zcode-keydiag.json; echo; echo
}

CHAT='{"model":"glm-5.3","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}'
MSG='{"model":"glm-5.3","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}'

# 1. Key liveness on the plain OpenAI-compatible surface (no plan/coding headers).
probe "GET /api/paas/v4/models (key liveness, open.bigmodel.cn)" \
  "https://open.bigmodel.cn/api/paas/v4/models" ""

# 1b. Same probe on the Z.ai (international) host — a key issued there is
#     REJECTED by open.bigmodel.cn with exactly the 401 we keep seeing.
probe "GET api.z.ai/api/paas/v4/models (Z.ai international)" \
  "https://api.z.ai/api/paas/v4/models" ""

# 2. The login-validation endpoint (quota envelope).
probe "GET /api/monitor/usage/quota/limit (login validation)" \
  "https://open.bigmodel.cn/api/monitor/usage/quota/limit" ""

# 3. A real model call on the OpenAI surface.
probe "POST /api/paas/v4/chat/completions" \
  "https://open.bigmodel.cn/api/paas/v4/chat/completions" "$CHAT"

# 4. The zcode target path with the full ZCode fingerprint (dual-write auth).
probe "POST /api/anthropic/v1/messages (zcode path + fingerprint)" \
  "https://open.bigmodel.cn/api/anthropic/v1/messages" "$MSG" \
  -H "x-api-key: $KEY" -H "anthropic-version: 2023-06-01" \
  -H "user-agent: ZCode/3.14.0 ai-sdk/provider-utils/4.0.27 runtime/node.js/24" \
  -H "http-referer: https://zcode.z.ai" -H "x-title: Z Code@cli" \
  -H "x-zcode-app-version: 3.14.0" -H "x-zcode-agent: glm" \
  -H "x-zcode-session-type: main" -H "x-release-channel: production"

echo "=== how to read this ==="
cat <<'EOF'
* ALL endpoints 401 令牌已过期或验证不正确  -> the credential itself is
  wrong: truncated copy, masked value, deleted/expired key, or wrong account.
  Re-copy from https://bigmodel.cn/apikey/platform (Coding Plan keys are also
  managed from the coding-plan page) and paste into a text editor first to
  confirm the full length.
* api.z.ai probe OK but open.bigmodel.cn 401 -> the key belongs to the Z.ai
  (international) account. It cannot be used on open.bigmodel.cn; either use a
  BigModel-issued key, or point the provider at api.z.ai (a config-only change).
* /models + /chat/completions OK but /api/anthropic 401 -> the key is alive but
  not entitled to the Anthropic/Coding-Plan surface; check the subscription.
* Only quota/limit fails with the 200-envelope 401 -> unusual; key is alive
  elsewhere. Report the outputs.
EOF
