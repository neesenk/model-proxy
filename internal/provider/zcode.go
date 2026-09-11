package provider

import (
	crand "crypto/rand"
	"fmt"
	"io"
	"model-proxy/internal/display"
	"model-proxy/internal/upstreamproxy"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// zcodeAppVersion is the ZCode desktop version whose client fingerprint this
// provider reproduces. Re-probed from /Applications/ZCode.app v3.11.2
// (2026-09-04 update; the 2026-07-20 spec probed 3.3.6 — same fingerprint
// schema, plus the new X-ZCode-Agent header).
const zcodeAppVersion = "3.11.2"

// zcodeUserAgentSuffix is what the ZCode agent engine (glm/zcode.cjs, Vercel
// AI SDK provider-utils) appends to User-Agent on chat-path requests.
// Packet captures (2026-09-11): the DESKTOP engine sends
// "ZCode/3.11.2 ai-sdk/provider-utils/4.0.27 runtime/node.js/24" (Electron 41
// → node 24); the standalone CLI bundle sends the same prefix with
// "runtime/node.js/22". We impersonate the desktop, hence node.js/24.
const zcodeUserAgentSuffix = "ai-sdk/provider-utils/4.0.27 runtime/node.js/24"

// ZCodeProvider forwards to Zhipu BigModel's Anthropic endpoint presenting the
// ZCode desktop client fingerprint, so a Coding Plan API key gets the plan's
// quota treatment (0.67 consumption coefficient + official-client priority).
//
// It mirrors ZhipuProvider — same BigModel backend, same quota envelope — but
// differs in two ways grounded in live probes of ZCode 3.3.6 and 3.11.2:
//   - AuthHeaders sends BOTH Authorization: Bearer and x-api-key (ZCode sends
//     both in both versions; zhipu sends Bearer and deletes x-api-key).
//   - ExtraHeaders sets anthropic-version + the ZCode fingerprint, including
//     3.11.2's new X-ZCode-Agent: glm header (sent unconditionally on the
//     main chat path; see buildZCodeSourceHeadersFromContext callers).
//
// See docs/superpowers/specs/2026-07-20-zcode-provider-design.md.
type ZCodeProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string

	sessionMu sync.Mutex
	sessionID string // stable per-process UUID for X-Session-Id (lazy init)
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
// x-api-key — ZCode sends both in 3.3.6 and 3.11.2
// (buildAnthropicConnectivityAuthHeaders, probed from the desktop binary; both
// branches return the same pair). This diverges from zhipu, which sends Bearer
// and deletes x-api-key.
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

// ExtraHeaders sets anthropic-version + the ZCode client fingerprint (probed
// from ZCode 3.11.2 buildZCodeSourceHeadersFromContext + the main chat path,
// which always appends X-ZCode-Agent: glm, and packet-captured 2026-09-11 —
// see docs/backend-contracts.md "zcode 契约"). It runs last in the forward path
// (after the client-UA whitelist copy and prov.Headers), so it overrides the
// client's forwarded User-Agent. X-Title uses sourceTitle "electron" (ZCode's
// desktop processes; its CLI sends "Z Code@cli" instead). X-Device-Mid is
// omitted (ZCode only sends it when telemetry-state.json has a deviceMid; see
// spec §3.2 / §9).
func (p *ZCodeProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	// Packet capture 2026-09-11: the chat path's UA is the ZCode UA plus the AI
	// SDK's runtime suffix (provider-utils appends " ai-sdk/… runtime/…"), NOT
	// the bare "ZCode/<ver>" of the connectivity probe. runtime/node.js/24
	// matches ZCode 3.11.2's Electron 41 node runtime (desktop capture; the
	// standalone CLI bundle sends node.js/22).
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion+" "+zcodeUserAgentSuffix)
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai")
	req.Header.Set("X-Title", "Z Code@electron")
	req.Header.Set("X-ZCode-App-Version", zcodeAppVersion)
	// New in 3.11.2: the main chat path appends this unconditionally
	// (zcode.cjs x4i: GPt({...csn(...), "X-ZCode-Agent":"glm"}, r)).
	req.Header.Set("X-ZCode-Agent", "glm")
	// Packet capture 2026-09-11: every desktop request carries a per-request
	// X-Request-Id and a per-session X-Session-Id UUID (withRequestIdHeader +
	// session scope). Synthetic v4 UUIDs reproduce the shape; the values are
	// ours. X-Session-Id is stable per proxy process (closest analogue of the
	// app's per-session id); it is NOT reset on key Refresh.
	req.Header.Set("X-Request-Id", newZCodeUUID())
	req.Header.Set("X-Session-Id", p.zcodeSessionID())
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
	req, _ := http.NewRequest("GET", p.cfg.UsageURL, nil)
	if err := p.AuthHeaders(req); err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: upstreamproxy.AutoTransport()}).Do(req)
	if err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	s, _ := ParseZhipuQuota(body, "")
	if s == nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "not zhipu quota format"}, nil
	}
	return s, nil
}

// Usage prints "Provider:  zcode" first (interface contract), then the parsed
// quota snapshot. Byte-for-byte the zhipu display logic (same BigModel backend).
func (p *ZCodeProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	req, _ := http.NewRequest("GET", p.cfg.UsageURL, nil)
	if err := p.AuthHeaders(req); err != nil {
		fmt.Println(display.Yellow("Not logged in.") + " Run: " + display.Cyan("model-proxy login "+p.providerName))
		return nil
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: upstreamproxy.AutoTransport()}).Do(req)
	if err != nil {
		fmt.Println(display.Red("Error: usage request: " + err.Error()))
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", display.Red("Error:"), resp.StatusCode, display.Truncate(string(body), 200))
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

// zcodeSessionID lazily mints the stable per-process X-Session-Id value.
func (p *ZCodeProvider) zcodeSessionID() string {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.sessionID == "" {
		p.sessionID = newZCodeUUID()
	}
	return p.sessionID
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

// resolveClientLanguage returns a printable locale (LC_ALL/LC_MESSAGES/LANG) or
// "unknown" — mirrors ZCode's Intl locale fallback.
func resolveClientLanguage() string {
	for _, env := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := printableASCII(os.Getenv(env)); v != "" {
			return v
		}
	}
	return "unknown"
}

// resolveClientTimezone returns a printable timezone (TZ env) or "unknown".
func resolveClientTimezone() string {
	if v := printableASCII(os.Getenv("TZ")); v != "" {
		return v
	}
	return "unknown"
}
