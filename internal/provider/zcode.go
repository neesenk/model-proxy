package provider

import (
	crand "crypto/rand"
	"crypto/sha256"
	"fmt"
	"model-proxy/internal/display"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
)

// zcodeAppVersion is the ZCode client version whose fingerprint this provider
// reproduces. ZCode was open-sourced on 2026-09-21 (github.com/zai-org/ZCode,
// Apache-2.0, root package.json version = 3.14.0), so this is now source
// verified instead of reverse-engineered. Cross-checked against:
//   - packages/shared/src/zcode-source-headers.ts (base fingerprint builder)
//   - apps/zcode-cli/packages/bootstrap/src/model-config.ts (CLI chat path:
//     X-ZCode-Agent: glm + sourceTitle cli/electron)
//   - apps/zcode-cli/packages/bootstrap/src/runtime-platform-headers.ts
//     (X-Platform / X-Os-Category / X-Os-Version)
//   - apps/zcode-cli/packages/adapters/src/model/runner-attribution.ts
//     (x-request-id / x-session-id / x-query-id / x-zcode-trace-id /
//     x-zcode-session-type)
//
// plus the 2026-09-11 mitmproxy capture of ZCode 3.11.2 (see
// docs/backend-contracts.md "zcode 契约").
const zcodeAppVersion = "3.14.0"

// AI-SDK User-Agent segments, in the order the SDKs append them:
// @ai-sdk/anthropic@3.0.81 appends its own segment when createAnthropic builds
// the provider (getHeaders → withUserAgentSuffix), then provider-utils appends
// its segment plus the runtime tag when the request is issued. Node >= 21.1
// (the CLI engine requires node >= 24) resolves the runtime via
// navigator.userAgent, yielding "runtime/node.js/<major>" — which is exactly
// what the 2026-09-11 capture recorded ("runtime/node.js/24"). The 3.11.2
// capture predates @ai-sdk/anthropic v3 and lacks the ai-sdk/anthropic
// segment; the open-sourced 3.14.0 code path emits it, so we match the source.
const (
	zcodeAnthropicSdkSuffix = "ai-sdk/anthropic/3.0.81"
	zcodeUserAgentSuffix    = "ai-sdk/provider-utils/4.0.27 runtime/node.js/24"
)

// zcodeSourceTitle is ZCode's sourceTitle for the standalone CLI engine
// (model-config.ts detectDefaultProviderSourceTitle: argv without
// app-server/agent-server → "cli"; the desktop's agent-server → "electron").
// The apikey Coding-Plan path this provider simulates is the CLI's built-in
// bigmodel-api template (access: zhipu-coding-plan-api-key, baseUrl
// open.bigmodel.cn/api/anthropic), so we impersonate the CLI, not the desktop
// (the desktop agent-server carries the OAuth/JWT subscription path instead).
const zcodeSourceTitle = "cli"

// ZCodeProvider forwards to Zhipu BigModel's Anthropic endpoint presenting the
// ZCode CLI client fingerprint (sourceTitle "cli"), so a Coding Plan API key
// gets the plan's quota treatment (0.67 consumption coefficient +
// official-client priority).
//
// It mirrors ZhipuProvider — same BigModel backend, same quota envelope — but
// differs in two ways, both source-verified against the open-sourced ZCode
// 3.14.0 (github.com/zai-org/ZCode):
//   - AuthHeaders sends BOTH Authorization: Bearer and x-api-key
//     (model-execution.ts: createAnthropic sends x-api-key,
//     withAnthropicAuthorizationHeader adds Bearer; zhipu sends Bearer and
//     deletes x-api-key).
//   - ExtraHeaders sets anthropic-version + the ZCode fingerprint
//     (zcode-source-headers.ts + model-config.ts + runner-attribution.ts),
//     including the request-attribution id headers.
//
// See docs/backend-contracts.md "zcode 契约" for the full source cross-check.
type ZCodeProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string

	sessionMu sync.Mutex
	sessionID string // fallback X-Session-Id when the client sends no session hint (lazy init)
}

func init() {
	Register("zcode", func(cfg *Config, providerName string) (Provider, error) {
		return &ZCodeProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// AuthHeaders injects the Coding Plan API key as BOTH Authorization: Bearer and
// x-api-key. Source-verified in the open-sourced ZCode 3.14.0
// (model-execution.ts): createAnthropic's apiKey makes the AI SDK send
// x-api-key, and withAnthropicAuthorizationHeader adds the Bearer header when
// none is configured — the same pair the 3.3.6/3.11.2 bundles sent. This
// diverges from zhipu, which sends Bearer and deletes x-api-key.
func (p *ZCodeProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// Refresh clears the cached key on a 401 (delegates to the embedded base;
// p.ApiKeyBase explicit to avoid self-recursion).
func (p *ZCodeProvider) Refresh() error { return p.ApiKeyBase.Refresh() }

// Logout removes the apikey file/pool entry (mirrors ZhipuProvider.Logout).
func (p *ZCodeProvider) Logout() error { return p.DeleteKey() }

// RewriteRequest is a no-op: zcode is same-protocol Anthropic passthrough.
func (p *ZCodeProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

// FetchModels lists models via the OpenAI base (/models), Bearer-authed.
func (p *ZCodeProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// ProbeRequest: zcode speaks the Anthropic messages API, so the probe goes to
// /v1/messages with an anthropic body (mirrors forward's anthropic path + aqp).
func (p *ZCodeProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   AnthropicProbeBody(modelID),
	}
}

// ExtraHeaders sets anthropic-version + the ZCode client fingerprint. Every
// value is source-verified against the open-sourced ZCode 3.14.0:
//   - zcode-source-headers.ts (buildZCodeSourceHeadersFromContext): the
//     header set + printable-ASCII normalization + "unknown" fallbacks +
//     osCategory mapping + X-Device-Mid only when telemetry-state.json has one.
//   - model-config.ts (buildCliZCodeSourceHeaders, the CLI chat path):
//     X-ZCode-Agent: glm + X-Title "Z Code@<sourceTitle>" + release channel.
//   - runtime-platform-headers.ts: X-Platform / X-Os-Category / X-Os-Version.
//   - runner-attribution.ts (createModelRequestAttributionHeaders): the
//     per-request attribution id headers.
//   - model-execution.ts: the AI SDK supplies anthropic-version 2023-06-01 and
//     the x-api-key/Authorization dual write (see AuthHeaders).
//
// It runs last in the forward path (after the client-UA whitelist copy and
// prov.Headers), so it overrides the client's forwarded User-Agent.
// X-Device-Mid is omitted (ZCode only sends it when telemetry-state.json has a
// deviceMid).
func (p *ZCodeProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	// Chat-path UA: "ZCode/<ver>" + the two AI-SDK segments, in append order.
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion+" "+zcodeAnthropicSdkSuffix+" "+zcodeUserAgentSuffix)
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai")
	req.Header.Set("X-Title", "Z Code@"+zcodeSourceTitle)
	req.Header.Set("X-ZCode-App-Version", zcodeAppVersion)
	req.Header.Set("X-ZCode-Agent", "glm")
	// Attribution ids (runner-attribution.ts): x-request-id is a fresh v4 UUID
	// per model call (and per retry attempt — createAttemptStatusContext);
	// x-zcode-session-type is a derived enum the Coding Plan server uses to
	// tell main/subagent/other traffic apart (we forward main-agent traffic);
	// x-zcode-trace-id is a fresh v4 UUID per trace. x-query-id and x-session-id
	// are keyed off the client's own interaction/session ids when present so
	// distinct conversations stay distinct on the wire — the real CLI mints
	// these itself (query_<uuid>/sess_<uuid>) and strips the internal prefixes
	// before sending, so the wire values are bare UUIDs either way.
	req.Header.Set("X-Request-Id", newZCodeUUID())
	req.Header.Set("X-ZCode-Session-Type", "main")
	req.Header.Set("X-ZCode-Trace-Id", newZCodeUUID())
	req.Header.Set("X-Query-Id", zcodeQueryID(req))
	req.Header.Set("X-Session-Id", p.zcodeSessionID(req))
	req.Header.Set("X-Platform", nodePlatform(runtime.GOOS)+"-"+nodeArch(runtime.GOARCH))
	req.Header.Set("X-Release-Channel", "production")
	req.Header.Set("X-Client-Language", resolveClientLanguage())
	req.Header.Set("X-Client-Timezone", resolveClientTimezone())
	req.Header.Set("X-Os-Category", osCategory(runtime.GOOS))
	if v := osVersion(); v != "" {
		req.Header.Set("X-Os-Version", v)
	}
}

// Quota GETs the BigModel usage_url and parses the quota envelope via
// ParseZhipuQuota (zcode IS BigModel — same envelope). On any failure returns a
// BillingUnknown snapshot carrying the error (never a non-nil error), mirroring
// ZhipuProvider.Quota so the scheduler poll stays alive.
func (p *ZCodeProvider) Quota() (*QuotaSnapshot, error) {
	return bigmodelQuota(p.cfg.UsageURL, p.AuthHeaders, p.cfg.Headers)
}

// Usage prints "Provider:  zcode" first (interface contract), then the parsed
// quota snapshot. Byte-for-byte the zhipu display logic (same BigModel backend).
func (p *ZCodeProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	body, ok := usageGetForDisplay(p.cfg.UsageURL, p.providerName, p.AuthHeaders, p.cfg.Headers)
	if !ok {
		return nil
	}
	if s, _ := ParseZhipuQuota(body, ""); s != nil {
		if s.Level != "" {
			fmt.Printf("%s %s\n", display.Dim("Level:     "), display.Magenta(s.Level))
		}
		DecorateExhaustionEta(p.providerName, s)
		printQuotaSnapshot(s)
		return nil
	}
	fmt.Println(display.Yellow("Quota unavailable (not BigModel format)."))
	return nil
}

// ---- fingerprint helpers (Node-name mappings + printable guards) ----

// newZCodeUUID returns an RFC 4122 v4 UUID string (crypto/rand). Hand-rolled:
// google/uuid is only an indirect dep and promoting it for two headers is not
// worth it. On the (unreachable in practice) rand error the bytes stay zeroed
// but the string remains well-formed.
func newZCodeUUID() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// zcodeSessionID returns the X-Session-Id value for this request. ZCode's CLI
// mints one session id per CLI session (createSessionId = sess_<uuid>) and
// strips the internal prefix before putting it on the wire, so the observable
// value is a bare v4 UUID. A proxy process outlives any single conversation,
// so we derive the id from the client's own session identifiers (whitelisted
// onto the upstream request by targetexec before ExtraHeaders runs):
// distinct client sessions get distinct ids, one session stays stable, and the
// value survives proxy restarts. Without any client hint (direct API clients,
// probes) we fall back to a process-stable random UUID.
// NOTE: header names are Go-canonical — the whitelist's "user_id" lands as
// "User_id" (underscore is not a canonicalization separator), hence the
// spelling below.
func (p *ZCodeProvider) zcodeSessionID(req *http.Request) string {
	for _, h := range []string{"X-Claude-Code-Session-Id", "X-Session-Id", "User_id"} {
		if v := printableASCII(req.Header.Get(h)); v != "" {
			return zcodeUUIDFromKey("session:" + h + ":" + v)
		}
	}
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.sessionID == "" {
		p.sessionID = newZCodeUUID()
	}
	return p.sessionID
}

// zcodeQueryID returns the X-Query-Id value for this request. ZCode's query id
// is per user query (createQueryId = query_<uuid>, prefix stripped on the
// wire). Claude Code's X-Interaction-Id identifies exactly one user
// interaction, so we derive a stable id from it when present (all model calls
// of one interaction share it, matching the real per-query scope); otherwise a
// fresh id per request.
func zcodeQueryID(req *http.Request) string {
	if v := printableASCII(req.Header.Get("X-Interaction-Id")); v != "" {
		return zcodeUUIDFromKey("query:" + v)
	}
	return newZCodeUUID()
}

// zcodeUUIDFromKey derives a stable RFC 4122 *v4-shaped* UUID from an opaque
// key (sha256 → first 16 bytes, version/variant bits forced). The real ZCode
// ids are crypto.randomUUID() v4 values; a hash formatted the same way is
// indistinguishable on the wire while keeping one client session/interaction
// mapped to exactly one id across processes.
func zcodeUUIDFromKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// nodePlatform maps Go GOOS to Node's process.platform naming (windows→win32).
func nodePlatform(goos string) string {
	switch goos {
	case "windows":
		return "win32"
	default:
		return goos // darwin, linux already match
	}
}

// nodeArch maps Go GOARCH to Node's process.arch naming (amd64→x64, 386→ia32).
func nodeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		return goarch // arm64 matches
	}
}

// osCategory mirrors ZCode's normalizeOsCategory: darwin→macos, win32→windows,
// else linux.
func osCategory(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	case "windows":
		return "windows"
	default:
		return "linux"
	}
}

// printableASCII mirrors ZCode's normalizePrintableHeaderValue: returns the
// trimmed value only if non-empty and entirely ASCII-printable ([\x20-\x7e]);
// otherwise "" (callers then emit "unknown" or omit the header).
func printableASCII(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return s
}

// resolveClientLanguage mirrors ZCode's Intl-based lookup: the real CLI reads
// Intl.DateTimeFormat().resolvedOptions().locale, which yields a BCP-47 tag
// (language[-REGION], e.g. "zh-CN"/"en-US") and never carries a codeset. Go
// has no ICU default-locale accessor, so we approximate from the same POSIX
// variables ICU itself reads (LC_ALL/LC_MESSAGES/LANG) and normalize the value
// into BCP-47 shape: "en_US.UTF-8" → "en-US". "C"/"POSIX" carry no language
// and are skipped. Anything non-printable falls through to "unknown", which is
// also ZCode's own fallback.
func resolveClientLanguage() string {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := printableASCII(normalizeLocale(os.Getenv(env))); v != "" {
			return v
		}
	}
	return "unknown"
}

// normalizeLocale converts a POSIX locale value to the BCP-47 shape
// Intl resolves to: drop the codeset/modifier (".UTF-8", "@euro"), split on
// "_", lowercase the language subtag and uppercase the rest. Returns "" for
// values that carry no language (empty, "C", "POSIX", malformed).
func normalizeLocale(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".@"); i >= 0 {
		s = s[:i]
	}
	if s == "" || s == "C" || s == "POSIX" {
		return ""
	}
	parts := strings.Split(s, "_")
	for i, part := range parts {
		if part == "" {
			return ""
		}
		if i == 0 {
			parts[i] = strings.ToLower(part)
		} else {
			parts[i] = strings.ToUpper(part)
		}
	}
	return strings.Join(parts, "-")
}

// resolveClientTimezone mirrors Intl.DateTimeFormat().resolvedOptions().timeZone:
// the TZ env when set (leading ":" stripped, POSIX-style), else the system
// timezone ICU would pick up, else "unknown".
func resolveClientTimezone() string {
	if v := printableASCII(strings.TrimPrefix(strings.TrimSpace(os.Getenv("TZ")), ":")); v != "" {
		return v
	}
	if v := systemTimezone(); v != "" {
		return v
	}
	return "unknown"
}

// systemTimezone reads the /etc/localtime symlink the way ICU resolves the
// system zone: both the /usr/share/zoneinfo/<Area>/<City> (linux) and
// /var/db/timezone/zoneinfo/<Area>/<City> (darwin) layouts reduce to the IANA
// name after the "zoneinfo/" segment. Returns "" when /etc/localtime is not a
// symlink (some minimal images) or unreadable.
func systemTimezone() string {
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	if i := strings.Index(target, "zoneinfo/"); i >= 0 {
		target = target[i+len("zoneinfo/"):]
	}
	return printableASCII(target)
}
