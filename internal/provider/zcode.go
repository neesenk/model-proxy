package provider

import (
	"fmt"
	"io"
	"model-proxy/internal/display"
	"model-proxy/internal/upstreamproxy"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// zcodeAppVersion is the ZCode desktop version whose client fingerprint this
// provider reproduces. Probed from /Applications/ZCode.app v3.3.6.
const zcodeAppVersion = "3.3.6"

// ZCodeProvider forwards to Zhipu BigModel's Anthropic endpoint presenting the
// ZCode desktop client fingerprint, so a Coding Plan API key gets the plan's
// quota treatment (0.67 consumption coefficient + official-client priority).
//
// It mirrors ZhipuProvider — same BigModel backend, same quota envelope — but
// differs in two ways grounded in a live probe of ZCode 3.3.6:
//   - AuthHeaders sends BOTH Authorization: Bearer and x-api-key (ZCode sends
//     both; zhipu sends Bearer and deletes x-api-key).
//   - ExtraHeaders sets anthropic-version + the 10-header ZCode fingerprint.
//
// See docs/superpowers/specs/2026-07-20-zcode-provider-design.md.
type ZCodeProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
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
// x-api-key — ZCode 3.3.6 sends both (buildAnthropicConnectivityAuthHeaders,
// probed from the desktop binary). This diverges from zhipu, which sends Bearer
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
// from ZCode 3.3.6 buildZCodeSourceHeaders). It runs last in the forward path
// (after the client-UA whitelist copy and prov.Headers), so it overrides the
// client's forwarded User-Agent. X-Device-Mid is omitted (ZCode omits it when
// unset; see spec §3.2 / §9).
func (p *ZCodeProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai")
	req.Header.Set("X-Title", "Z Code@electron")
	req.Header.Set("X-ZCode-App-Version", zcodeAppVersion)
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
