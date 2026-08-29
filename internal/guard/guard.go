// Package guard owns the outbound request-body secret scan (DLP-lite): a
// table of high-confidence secret patterns (embedded rules.json, curated from
// the pinned gitleaks default rule set plus hand-written rules) matched
// against the raw client body before it is forwarded upstream.
//
// The table deliberately errs on the side of missing a leak ("宁漏勿滥"):
// every rule requires an issuer-specific literal AND (for upstream rules with
// an entropy threshold) a minimum Shannon entropy, so ordinary code content
// (variable names, short ids, docs) does not match. It is a safety net for
// obvious accidents, not a general DLP engine.
//
// Scan happens in two phases: a pure bytes.Contains literal prefilter (clean
// bodies — the vast majority — run zero regexes and zero decodes), then a
// precise pass limited to rules whose literals hit. The precise pass covers
// plaintext regex matches, exact known-secret values and their encoded
// variants (base64/hex/url-escaped), an encoded-literal channel that decodes
// bounded token spans and re-runs the owning rule, user custom patterns, and
// a sensitive-path signal table (ScanPaths, plus its context-aware form
// ScanPathsContext which splits hits into strong tool-call positions and weak
// prose mentions).
//
// Scan/Redact/ScanPaths/ScanPathsContext report rule TYPE NAMES and path
// CATEGORY NAMES only.
// Matched secret bytes are never returned beyond the redacted body itself, so
// callers can log/count hits without any credential reaching logs, events, or
// test output.
package guard

// RedactPlaceholder replaces every matched secret in the redacted body.
const RedactPlaceholder = "[REDACTED]"
