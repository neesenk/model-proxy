#!/usr/bin/env bash
# sync_guard_rules.sh — re-extract the curated gitleaks rule selection in
# internal/guard/rules.json from an upstream gitleaks release and diff the
# regenerated candidate against the checked-in file.
#
# The script NEVER overwrites rules.json: it writes the candidate to a temp
# file and shows a unified diff. Review the diff (regex/entropy drift,
# dropped upstream rules), then replace internal/guard/rules.json manually
# and update the rule fixtures in internal/guard/scanner_rules_test.go.
#
# Usage:
#   scripts/sync_guard_rules.sh [gitleaks-tag]
#
#   gitleaks-tag   upstream tag to pull config/gitleaks.toml from
#                  (default: the pin recorded in rules.json's upstream field).
#
# Selection criteria for the extraction (see internal/guard/rules.go): a
# carried gitleaks rule keeps its curated name and literals; regex and
# entropy are re-pulled from the upstream rule whose id matches our
# source_rule. Rules with source "model-proxy" pass through untouched.
#
# Dependencies: bash, curl, python3 >= 3.11 (stdlib tomllib).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RULES_JSON="$ROOT/internal/guard/rules.json"

for cmd in curl python3; do
  command -v "$cmd" >/dev/null 2>&1 || { echo "error: $cmd not found" >&2; exit 1; }
done
python3 -c 'import tomllib' 2>/dev/null || {
  echo "error: python3 >= 3.11 with stdlib tomllib is required" >&2
  exit 1
}

TAG="${1:-}"
if [[ -z "$TAG" ]]; then
  TAG="$(python3 - "$RULES_JSON" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    upstream = json.load(f)["upstream"]
# "gitleaks v8.28.0" -> "v8.28.0"
print(upstream.split()[-1])
PY
)"
fi
# TAG is interpolated into URL paths and query strings below; reject anything
# outside [A-Za-z0-9._-] so '/', '?', '#' etc. cannot inject into the URL.
if [[ ! "$TAG" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "error: invalid gitleaks tag '$TAG': only [A-Za-z0-9._-] allowed" >&2
  exit 1
fi
echo "upstream tag: $TAG" >&2

TOML="$(mktemp -t gitleaks.toml.XXXXXX)"
CANDIDATE="$(mktemp -t rules.json.candidate.XXXXXX)"
trap 'rm -f "$TOML"' EXIT

RAW_URL="https://raw.githubusercontent.com/gitleaks/gitleaks/${TAG}/config/gitleaks.toml"
if ! curl -fsSL --retry 2 -o "$TOML" "$RAW_URL"; then
  echo "raw.githubusercontent.com unreachable, trying api.github.com contents API" >&2
  API_URL="https://api.github.com/repos/gitleaks/gitleaks/contents/config/gitleaks.toml?ref=${TAG}"
  curl -fsSL --retry 2 -H "Accept: application/vnd.github+json" "$API_URL" \
    | python3 -c '
import base64, json, sys
payload = json.load(sys.stdin)
sys.stdout.buffer.write(base64.b64decode(payload["content"]))
' > "$TOML"
fi

python3 - "$TOML" "$RULES_JSON" "$CANDIDATE" <<'PY'
import json, sys, tomllib

toml_path, rules_path, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
with open(toml_path, "rb") as f:
    doc = tomllib.load(f)
upstream = {r["id"]: r for r in doc.get("rules", [])}
with open(rules_path) as f:
    cur = json.load(f)

missing, drift = [], []
for entry in cur["rules"]:
    if entry["source"] != "gitleaks":
        continue
    up = upstream.get(entry["source_rule"])
    if up is None:
        missing.append(entry["source_rule"])
        continue
    if "regex" in up:
        if up["regex"] != entry["regex"]:
            drift.append(entry["source_rule"])
        entry["regex"] = up["regex"]
    entry["entropy"] = None
    if "entropy" in up:
        e = float(up["entropy"])
        entry["entropy"] = int(e) if e == int(e) else e

for rid in missing:
    print(f"warning: {rid!r} no longer exists upstream; entry kept as-is", file=sys.stderr)
for rid in drift:
    print(f"note: {rid!r} regex changed upstream (or was locally refined)", file=sys.stderr)

with open(out_path, "w") as f:
    json.dump(cur, f, indent=2, ensure_ascii=False)
    f.write("\n")
PY

echo "candidate written to: $CANDIDATE" >&2
if diff -u "$RULES_JSON" "$CANDIDATE"; then
  echo "rules.json is already in sync with gitleaks $TAG" >&2
else
  cat >&2 <<EOF

diff shown above (left: checked-in rules.json, right: regenerated candidate).
To adopt: review every hunk, then
  cp "$CANDIDATE" "$RULES_JSON"
and run: go test ./internal/guard/ -count=1
EOF
fi
