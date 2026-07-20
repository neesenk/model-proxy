# zcode Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `zcode` provider that forwards to Zhipu BigModel's Anthropic endpoint with ZCode 3.3.6's client fingerprint, so a Coding Plan API key gets the plan's quota treatment.

**Architecture:** A new apikey-style provider (`provider/zcode.go`) mirroring `zhipu` (same BigModel backend + quota envelope) with two provider-specific overrides grounded in a live probe of ZCode 3.3.6: (1) `AuthHeaders` sends **both** `Authorization: Bearer` and `x-api-key`; (2) `ExtraHeaders` sets `anthropic-version` + the 10-header ZCode fingerprint. Plus small wiring (config whitelist, login case, CLI labels) and docs.

**Tech Stack:** Go (stdlib only), white-box tests (`package provider`) with `net/http/httptest`, no testify.

## Global Constraints

- Ground truth is the live probe of `/Applications/ZCode.app` v3.3.6, recorded in `docs/superpowers/specs/2026-07-20-zcode-provider-design.md` §2. Every header value below is copied verbatim from that probe.
- `zcodeAppVersion = "3.3.6"` (const).
- Auth: send **both** `Authorization: Bearer <key>` and `x-api-key: <key>` (ZCode sends both — diverges from zhipu which deletes `x-api-key`).
- `anthropic-version: 2023-06-01`.
- Fingerprint header values: `User-Agent=ZCode/3.3.6`, `HTTP-Referer=https://zcode.z.ai`, `X-Title=Z Code@electron`, `X-ZCode-App-Version=3.3.6`, `X-Platform=<nodePlatform>-<nodeArch>`, `X-Release-Channel=production`, `X-Client-Language`/`X-Client-Timezone`=ASCII-printable-or-`unknown`, `X-Os-Category=darwin→macos|windows→windows|linux→linux`, `X-Os-Version`=best-effort-or-omitted.
- Tests: exact-value assertions (no `Contains(x)||Contains(y)`); `-race` clean; 80% package coverage (`scripts/cover.sh`).
- `gofmt -l .` clean, `go vet ./...` clean.
- **No commits without explicit user request** (CLAUDE.md). Run tests; hold `git commit`.

## File Structure

| File | Responsibility |
|---|---|
| `provider/zcode.go` (new) | `ZCodeProvider` (struct, Register, Auth/Refresh/Logout/Rewrite/Probe/Extra/Quota/Usage/FetchModels) + fingerprint helpers (`nodePlatform`, `nodeArch`, `osCategory`, `printableASCII`, `resolveClientLanguage`, `resolveClientTimezone`, `osVersion`) |
| `provider/zcode_test.go` (new) | white-box tests for AuthHeaders, ExtraHeaders, ProbeRequest, Quota, helpers |
| `config.go` | add `zcode` to the `known` provider_id whitelist (:580) + error message (:582) |
| `login.go` | add `case "zcode":` (:56) → open `bigmodel.cn/login` + delegate to apikey flow |
| `main.go` | update `usage` string login list (:30); add `case "zcode":` to `quotaSourceLabel` (:963) |
| `CLI.md` | document the zcode login flow (§4) |
| `docs/backend-contracts.md` | fix the stale `anthropic_base_url …/v1` note (:42/:51) |

Reuse (no change): `ApiKeyBase` (key storage/pool), `newApiKeyBaseBound`, `fetchModelsBearer`, `ParseZhipuQuota`, `printQuotaSnapshot`, `anthropicProbeBody`, `baseProbe`, `buildOne` (pool binding generic), logout dispatch (pool path).

---

### Task 1: Provider skeleton + dual-header AuthHeaders

**Files:**
- Create: `provider/zcode.go`
- Test: `provider/zcode_test.go`

**Interfaces:**
- Produces: `ZCodeProvider` struct; `Register("zcode", …)`; `AuthHeaders` sets both `Authorization: Bearer` and `x-api-key`; `Refresh`/`Logout`/`RewriteRequest`/`FetchModels`.

- [ ] **Step 1: Write the failing test** (`provider/zcode_test.go`)

```go
package provider

import (
	"net/http"
	"testing"
)

func TestZCode_AuthHeaders_SendsBothBearerAndXApiKey(t *testing.T) {
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "sk-test-123")}
	req, _ := http.NewRequest("POST", "https://open.bigmodel.cn/api/anthropic/v1/messages", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-test-123")
	}
	if got := req.Header.Get("x-api-key"); got != "sk-test-123" {
		t.Errorf("x-api-key = %q, want %q (ZCode sends BOTH)", got, "sk-test-123")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestZCode_AuthHeaders ./provider/`
Expected: FAIL — `ZCodeProvider` not defined.

- [ ] **Step 3: Write minimal implementation** (`provider/zcode.go`)

```go
package provider

import (
	"net/http"
)

// zcodeAppVersion is the ZCode desktop version whose client fingerprint this
// provider reproduces. Probed from /Applications/ZCode.app v3.3.6.
const zcodeAppVersion = "3.3.6"

// ZCodeProvider forwards to Zhipu BigModel's Anthropic endpoint presenting the
// ZCode desktop client fingerprint, so a Coding Plan API key gets the plan's
// quota treatment. Mirrors ZhipuProvider (same BigModel backend + quota
// envelope) but: (1) sends BOTH Authorization: Bearer AND x-api-key (ZCode 3.3.6
// sends both), and (2) ExtraHeaders sets anthropic-version + the ZCode
// fingerprint. See docs/superpowers/specs/2026-07-20-zcode-provider-design.md.
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
// x-api-key — ZCode sends both (buildAnthropicConnectivityAuthHeaders, probed
// in 3.3.6). This diverges from zhipu, which sends Bearer and deletes x-api-key.
func (p *ZCodeProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

func (p *ZCodeProvider) Refresh() error { return p.ApiKeyBase.Refresh() }

// Logout removes the apikey file/pool entry (mirrors ZhipuProvider.Logout).
func (p *ZCodeProvider) Logout() error { return p.DeleteKey() }

// RewriteRequest is a no-op (same-protocol Anthropic passthrough).
func (p *ZCodeProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

// FetchModels lists models via the OpenAI base (/models), Bearer-authed.
func (p *ZCodeProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestZCode_AuthHeaders ./provider/`
Expected: PASS.

- [ ] **Step 5: Verify it compiles + vets**

Run: `cd model-proxy && go vet ./provider/`
Expected: no output (note: `Quota`/`Usage`/`ProbeRequest`/`ExtraHeaders` are not yet defined — they come from `baseProbe` defaults for now; the struct satisfies the interface via the embedded `baseProbe` for Probe/Extra/Filter, but `Quota`/`Usage` are still required → this task deliberately leaves them to Task 3, so `go vet`/`go build` will fail until Task 3. That's expected TDD progression within the file; if you want a green build between tasks, stub `Quota`/`Usage` here and replace in Task 3.)

> Implementer note: to keep `go build` green per task, add minimal stubs now and replace them in Task 3:
> ```go
> func (p *ZCodeProvider) Quota() (*QuotaSnapshot, error) { return &QuotaSnapshot{Billing: BillingUnknown}, nil }
> func (p *ZCodeProvider) Usage() error                   { return nil }
> ```
> (Task 3 overwrites these with the real implementations.)

- [ ] **Step 6: Commit** (skip — CLAUDE.md: commit only on request)

---

### Task 2: ZCode fingerprint ExtraHeaders + helpers

**Files:**
- Modify: `provider/zcode.go` (append ExtraHeaders + helpers)
- Test: `provider/zcode_test.go` (append)

**Interfaces:**
- Consumes: `zcodeAppVersion` (Task 1).
- Produces: `ExtraHeaders(req, path)` setting `anthropic-version` + the 10 ZCode headers; helpers `nodePlatform`, `nodeArch`, `osCategory`, `printableASCII`, `resolveClientLanguage`, `resolveClientTimezone`, `osVersion`.

- [ ] **Step 1: Write the failing tests** (append to `provider/zcode_test.go`)

```go
import (
	"net/http"
	"runtime"
	"testing"
)

func TestZCode_ExtraHeaders_Fingerprint(t *testing.T) {
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k")}
	req, _ := http.NewRequest("POST", "https://x/v1/messages", nil)
	p.ExtraHeaders(req, "/v1/messages")

	cases := map[string]string{
		"User-Agent":         "ZCode/3.3.6",
		"HTTP-Referer":       "https://zcode.z.ai",
		"X-Title":            "Z Code@electron",
		"X-ZCode-App-Version": "3.3.6",
		"X-Release-Channel":  "production",
		"anthropic-version":  "2023-06-01",
	}
	for h, want := range cases {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	// Runtime-derived (exact for THIS platform).
	if got, want := req.Header.Get("X-Platform"), nodePlatform(runtime.GOOS)+"-"+nodeArch(runtime.GOARCH); got != want {
		t.Errorf("X-Platform = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("X-Os-Category"), osCategory(runtime.GOOS); got != want {
		t.Errorf("X-Os-Category = %q, want %q", got, want)
	}
}

func TestZCode_NodeNameMappings(t *testing.T) {
	plat := map[string]string{"darwin": "darwin", "windows": "win32", "linux": "linux"}
	for goos, want := range plat {
		if got := nodePlatform(goos); got != want {
			t.Errorf("nodePlatform(%q) = %q, want %q", goos, got, want)
		}
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "386": "ia32"}
	for goarch, want := range arch {
		if got := nodeArch(goarch); got != want {
			t.Errorf("nodeArch(%q) = %q, want %q", goarch, got, want)
		}
	}
	cat := map[string]string{"darwin": "macos", "windows": "windows", "linux": "linux"}
	for goos, want := range cat {
		if got := osCategory(goos); got != want {
			t.Errorf("osCategory(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestZCode_PrintableASCIIGuard(t *testing.T) {
	for in, want := range map[string]string{
		"zh-CN":          "zh-CN",
		"  Asia/Shanghai ": "Asia/Shanghai",
		"中文":            "", // non-ASCII → rejected
		"":               "",
	} {
		if got := printableASCII(in); got != want {
			t.Errorf("printableASCII(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test -run 'TestZCode_ExtraHeaders|TestZCode_NodeNameMappings|TestZCode_PrintableASCIIGuard' ./provider/`
Expected: FAIL — `ExtraHeaders`/helpers undefined.

- [ ] **Step 3: Implement ExtraHeaders + helpers** (append to `provider/zcode.go`)

```go
import (
	"os"
	"runtime"
	"strings"
	"syscall"
)

// ExtraHeaders sets anthropic-version + the ZCode client fingerprint (probed
// from ZCode 3.3.6 buildZCodeSourceHeaders). It runs last in the forward path
// (after the client-UA whitelist copy and prov.Headers), so it overrides the
// client's forwarded User-Agent. X-Device-Mid is omitted (ZCode omits it when
// unset; see spec §3.2).
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

// osCategory mirrors ZCode's normalizeOsCategory: darwin→macos, win32→windows, else linux.
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
// trimmed value only if it is non-empty and entirely ASCII-printable
// ([\x20-\x7e]); otherwise "". Such headers are then sent as "unknown" or omitted.
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

// resolveClientLanguage returns a printable locale (from LC_ALL/LC_MESSAGES/LANG)
// or "unknown" — mirrors ZCode's Intl locale fallback.
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

// osVersion returns the OS product version on darwin (kern.osproductversion),
// else "" (header omitted — ZCode omits X-Os-Version when unavailable).
func osVersion() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return printableASCII(v)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestZCode_ExtraHeaders|TestZCode_NodeNameMappings|TestZCode_PrintableASCIIGuard' ./provider/`
Expected: PASS.

- [ ] **Step 5: Commit** (skip — CLAUDE.md)

---

### Task 3: ProbeRequest + Quota + Usage (real impls)

**Files:**
- Modify: `provider/zcode.go` (replace the Task-1 Quota/Usage stubs; add ProbeRequest)
- Test: `provider/zcode_test.go` (append)

**Interfaces:**
- Consumes: `ParseZhipuQuota`, `printQuotaSnapshot`, `anthropicProbeBody` (existing).
- Produces: `ProbeRequest(modelID)` → `/v1/messages`; `Quota()` → BigModel quota via `ParseZhipuQuota`; `Usage()` prints `Provider:  zcode` + snapshot.

- [ ] **Step 1: Write the failing tests** (append)

```go
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestZCode_ProbeRequest_AnthropicShape(t *testing.T) {
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k")}
	pr := p.ProbeRequest("glm-4.6")
	if pr.Method != http.MethodPost || pr.Path != "/v1/messages" {
		t.Errorf("ProbeRequest = %+v, want POST /v1/messages", pr)
	}
	var body map[string]any
	if err := json.Unmarshal(pr.Body, &body); err != nil {
		t.Fatalf("probe body not json: %v", err)
	}
	if body["model"] != "glm-4.6" {
		t.Errorf("probe model = %v, want glm-4.6", body["model"])
	}
}

func TestZCode_Quota_ParsesBigModelEnvelope(t *testing.T) {
	fixture := `{"success":true,"data":{"level":"tier-4","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":10,"usage":100000,"currentValue":10000,"remaining":90000,"nextResetTime":1750000000000}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("quota GET auth = %q", r.Header.Get("Authorization"))
		}
		w.Write([]byte(fixture))
	}))
	defer srv.Close()
	p := &ZCodeProvider{
		ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k"),
		cfg:        &Config{UsageURL: srv.URL},
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing = %v, want BillingPlan", s.Billing)
	}
	if len(s.Windows) != 1 || !s.Windows[0].Short || s.Windows[0].Duration != 5*time.Hour {
		t.Errorf("window markers wrong: %+v", s.Windows)
	}
}
```
(Add `"time"` to the test imports.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test -run 'TestZCode_ProbeRequest|TestZCode_Quota' ./provider/`
Expected: FAIL — `ProbeRequest` uses baseProbe default (`/chat/completions`); `Quota` is the stub.

- [ ] **Step 3: Implement** (replace the Task-1 `Quota`/`Usage` stubs; add `ProbeRequest` — append to `provider/zcode.go`)

```go
import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProbeRequest: zcode speaks the Anthropic messages API, so the probe goes to
// /v1/messages with an anthropic body (mirrors forward's anthropic path + aqp).
func (p *ZCodeProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   anthropicProbeBody(modelID),
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
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
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
// quota snapshot. Byte-for-byte the zhipu display logic (same backend).
func (p *ZCodeProvider) Usage() error {
	fmt.Printf("%s %s\n", Dim("Provider:  "), Bold(Blue(p.providerName)))
	req, _ := http.NewRequest("GET", p.cfg.UsageURL, nil)
	if err := p.AuthHeaders(req); err != nil {
		fmt.Println(Yellow("Not logged in.") + " Run: " + Cyan("model-proxy login "+p.providerName))
		return nil
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(Red("Error: usage request: " + err.Error()))
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", Red("Error:"), resp.StatusCode, Truncate(string(body), 200))
		return nil
	}
	if s, _ := ParseZhipuQuota(body, ""); s != nil {
		if s.Level != "" {
			fmt.Printf("%s %s\n", Dim("Level:     "), Magenta(s.Level))
		}
		printQuotaSnapshot(s)
		return nil
	}
	fmt.Println(Yellow("Quota unavailable (not BigModel format)."))
	return nil
}
```
(Consolidate the import block: the file ends up needing `fmt`, `io`, `net/http`, `os`, `runtime`, `strings`, `syscall`, `time`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test -run TestZCode ./provider/`
Expected: all PASS.

- [ ] **Step 5: Race + vet + fmt**

Run: `cd model-proxy && go test -race ./provider/ && go vet ./provider/ && gofmt -l provider/zcode.go provider/zcode_test.go`
Expected: tests pass, no vet output, no gofmt output.

- [ ] **Step 6: Commit** (skip — CLAUDE.md)

---

### Task 4: Wiring — config whitelist, login case, CLI labels

**Files:**
- Modify: `config.go:580` (whitelist) + `:582` (error msg)
- Modify: `login.go:56` (add `case "zcode":`)
- Modify: `main.go:30` (usage string) + `:963` (`quotaSourceLabel`)
- Test: `config_test.go` (append a zcode whitelist case)

**Interfaces:**
- Produces: config validation accepts `provider_id: zcode`; `login zcode` opens `bigmodel.cn/login` then runs the apikey flow; `doctor` labels zcode quota source `quota/limit`.

- [ ] **Step 1: Write the failing test** (append to `config_test.go`, near the existing whitelist/validation cases)

```go
func TestValidate_AcceptsZcodeProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"zcode": {Provider: "zcode", AnthropicBaseURL: "https://open.bigmodel.cn/api/anthropic"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("zcode provider_id rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestValidate_AcceptsZcodeProviderID .`
Expected: FAIL — `unknown provider_id "zcode"`.

- [ ] **Step 3: Wire the changes**

`config.go:580` — add `"zcode": true` to the `known` map:
```go
known := map[string]bool{"aqp": true, "codex": true, "zhipu": true, "deepseek": true, "volcengine": true, "kimi-code": true, "static": true, "zcode": true}
```
`config.go:582` — add `zcode` to the error message list:
```go
return fmt.Errorf("provider %q: unknown provider_id %q — valid: aqp, codex, zhipu, deepseek, volcengine, kimi-code, static, zcode", name, p.Provider)
```

`login.go:56` — add a `case "zcode":` in the `switch prov.Provider` (before `default:`):
```go
case "zcode":
    // BigModel Coding Plan: open the login page so the user can grab a Coding
    // Plan API key, then the standard apikey pool flow stores it.
    fmt.Println("Opening BigModel login to fetch a Coding Plan API key…")
    if err := openBrowser("https://bigmodel.cn/login"); err != nil {
        fmt.Fprintf(os.Stderr, "(could not open browser: %v — open https://bigmodel.cn/login manually)\n", err)
    }
    if err := runApiKeyLoginWithInput(cfg, provName, prov, "", label, replace); err != nil {
        log.Fatalf("login failed: %v", err)
    }
```
(Add `"os"` to login.go imports if not present — it already imports `os`.)

`main.go:30` — in the `usage` string, extend the `login <provider>` line's example list to include `zcode` (append-only, backward compatible — do not change existing text/exit codes; if the line names specific providers, add `zcode`).

`main.go:963` (`quotaSourceLabel`) — add:
```go
case "zcode":
    return "quota/limit"
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestValidate_AcceptsZcodeProviderID .`
Expected: PASS.

- [ ] **Step 5: Full build + test + vet + cover**

Run: `cd model-proxy && go build ./... && go test ./... && go vet ./... && gofmt -l .`
Expected: build OK, all tests pass, no vet output, no gofmt output.

- [ ] **Step 6: Coverage gate**

Run: `cd model-proxy && scripts/cover.sh`
Expected: every package ≥ 80%; `provider` package's `zcode.go` functions not in the below-threshold list.

- [ ] **Step 7: Commit** (skip — CLAUDE.md)

---

### Task 5: Docs — CLI.md, backend-contracts fix, config example

**Files:**
- Modify: `CLI.md` (§4 login section — document zcode, append-only)
- Modify: `docs/backend-contracts.md:42/:51` (fix stale `anthropic_base_url …/v1`)
- Reference: add a zcode block to the config example (if `defaults.go`/`config.yaml` carry provider examples) — append-only

- [ ] **Step 1: CLI.md §4** — add a zcode entry to the login dispatch description and a stdout/stderr contract block (mirror the codex block's format: what `login zcode` prints on stdout/stderr, exit 0/1). Keep it append-only (do not alter existing providers' lines).

Example block to add:
```markdown
### zcode — BigModel Coding Plan (API key)

逻辑：openBrowser("https://bigmodel.cn/login") → 用户登录后到 API Keys 拿 Coding Plan key
→ 走 apikey 池流程（prompt → validate usage_url → 存 <name>_apikeys.json）。

stdout:
  Opening BigModel login to fetch a Coding Plan API key…
  Enter API key for <name>: <stdin>
  Validating API key…            (stderr, 仅 usage_url 配置时)
  ✓ Saved account <masked> (<label>)
失败 → stderr `login failed: <err>` + exit 1。
```

- [ ] **Step 2: backend-contracts.md fix** — at :42 and :51, the `anthropic_base_url` is recorded with a trailing `/v1`; `config.go:585` rejects that (proxy keeps client `/v1`). Correct the prose so the canonical value is `https://open.bigmodel.cn/api/anthropic` (no `/v1`) for both zhipu and zcode; note DeepSeek's `/anthropic` base likewise carries no `/v1` in config (proxy appends `/v1/messages`).

- [ ] **Step 3: config example** — if `defaults.go` (the `config init` template) or `config.yaml` lists provider examples, append a commented `zcode:` block:
```yaml
  zcode:
    provider_id: zcode
    anthropic_base_url: https://open.bigmodel.cn/api/anthropic   # Coding Plan endpoint (no /v1)
    openai_base_url: https://open.bigmodel.cn/api/paas/v4        # /models listing only
    usage_url: https://open.bigmodel.cn/api/monitor/usage/quota/limit
    models: [glm-4.6, glm-4.5, glm-4.5-air, glm-4.7, glm-5.1]
```

- [ ] **Step 4: Build + test still green**

Run: `cd model-proxy && go build ./... && go test ./...`
Expected: PASS (docs/config-template changes shouldn't break tests; if `defaults.go` YAML is parsed by a test, ensure it still validates).

- [ ] **Step 5: Commit** (skip — CLAUDE.md)

---

## Self-Review

**Spec coverage:** §2 ground truth → Task 1 (dual auth) + Task 2 (fingerprint) + Task 3 (probe/anthropic-version via ExtraHeaders). §3 architecture → Tasks 1–3. §4 config/routing → Task 4 (whitelist) + Task 5 (example). §5 login → Task 4 (`case zcode`). §6 touch points → Tasks 4–5. §7 tests → Tasks 1–4. §8 risks → noted (coefficient verify is runtime, not code). All sections covered.

**Placeholder scan:** none — every code step shows actual code; the one "mirror zhipu" case (Usage) inlines the full body.

**Type consistency:** `ZCodeProvider` fields (`*ApiKeyBase`, `baseProbe`, `cfg *Config`, `providerName string`) match across tasks; `nodePlatform`/`nodeArch`/`osCategory`/`printableASCII` signatures match between Task 2 impl and tests; `ProbeRequest`/`Quota`/`Usage` signatures match the `Provider` interface.

## Execution

Plan complete and saved to `docs/superpowers/plans/2026-07-20-zcode-provider.md`. Per the active goal (执行计划), proceeding inline rather than offering a choice.
