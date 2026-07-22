# qwen-plan Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `qwen-plan` provider for the 千问 AI Token Plan 个人版 — dual-protocol OpenAI + Anthropic byte-level passthrough on Alibaba Bailian's token-plan maas gateway, one Bearer API key, with console-only Credits usage surfaced as an unmeasured quota + a subscription URL.

**Architecture:** A new `provider/qwen_plan.go` modeled on zhipu/deepseek (embeds `*ApiKeyBase` + `baseProbe`, dual-writes Bearer + x-api-key). Pure passthrough (no rewrite, no protocol hint). `FetchModels` reuses `fetchModelsBearer` against `/models`; `Quota()` returns `BillingUnknown` carrying the console URL in `Notes` (auto-serialized to `/api/status.quota` and rendered by `app.js` → no frontend change). Window exhaustion is handled reactively by the existing `failclass.go` 429 classification (`"Allocated quota exceeded"` → `rlQuota` → cooldown + failover). Login validates the key via a new generic `apiKeyValidationURL` fallback to `openai_base_url/models`, so no redundant `usage_url` is needed.

**Tech Stack:** Go 1.x, `net/http`, `net/http/httptest`, model-proxy provider framework, TDD.

## Global Constraints

- **Provider id:** `qwen-plan` (config `provider_id`, registered in `provider.New` via `init()`).
- **Auth:** every upstream request sets **both** `Authorization: Bearer <key>` and `x-api-key: <key>` (OpenAI endpoint ignores `x-api-key`; covers the Anthropic gateway's preference).
- **No `usage_url` in config.** Login validates the key via `openai_base_url/models` (generic fallback), not a dedicated field.
- **Anthropic base URL:** `https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic` — **no trailing `/v1`** (proxy keeps client's `/v1/messages`; `config.go` rejects `/v1`).
- **Quota:** `Quota()` returns `BillingUnknown` + `Notes` (console URL). Never makes a network call.
- **Credential file:** `~/.model-proxy/<name>_apikey.json` (`{api_key}`), like zhipu/deepseek/kimi-code.
- **Coverage:** `scripts/cover.sh` gate ≥ 80%. New state paths covered: success, auth, quota snapshot, login validation fallback.
- **Commits:** stage ONLY the files this plan creates/modifies (the worktree has unrelated uncommitted changes — leave them). Each task ends with a commit. Code + docs in the same commit (AGENTS.md rule 4).
- **Branch:** `feature/qwen-plan-provider` (already created).

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `model-proxy/provider/qwen_plan.go` | `QwenPlanProvider` struct, `init/Register`, `AuthHeaders`, `RewriteRequest`, `Logout`, `FetchModels`, `Quota`, console-URL const | Create |
| `model-proxy/provider/usage_display.go` | `QwenPlanProvider.Usage()` display method (matches the per-provider Usage convention) | Modify (append) |
| `model-proxy/provider/qwen_plan_test.go` | Auth dual-write, FetchModels parse+error, Quota snapshot+URL, Usage prints URL | Create |
| `model-proxy/config.go` | `known` allowlist + error/hint strings | Modify |
| `model-proxy/config_test.go` | `TestValidate_AcceptsQwenPlanProviderID` | Modify (append) |
| `model-proxy/login.go` | `apiKeyValidationURL` helper + wire into login core | Modify |
| `model-proxy/login_cmd_test.go` | `apiKeyValidationURL` fallback unit test | Modify (append) |
| `model-proxy/failclass_test.go` | 429 classification regression cases | Modify (append) |
| `model-proxy/defaults.go` | qwen-plan block in `defaultConfigYAML` | Modify |
| `model-proxy/config.yaml` | qwen-plan example block | Modify |
| `docs/backend-contracts.md` | qwen-plan contract section + naming-table row | Modify |
| `model-proxy/CLI.md` | login/usage dispatch prose | Modify |
| `model-proxy/README.md` | provider block + login/usage examples + credential table | Modify |
| `docs/decisions/intentional-behaviors.md` | "严禁 API 调用" restriction note | Modify |

---

### Task 1: Register the provider id in the config allowlist

**Files:**
- Modify: `model-proxy/config.go:618`, `:623`, `:625`
- Test: `model-proxy/config_test.go` (append `TestValidate_AcceptsQwenPlanProviderID`)

**Interfaces:** none (standalone validation).

- [ ] **Step 1: Write the failing test** — append to `model-proxy/config_test.go` (mirror `TestValidate_AcceptsZcodeProviderID`):

```go
func TestValidate_AcceptsQwenPlanProviderID(t *testing.T) {
	c := &Config{Listen: "127.0.0.1:8787", Providers: map[string]Provider{
		"qwen-plan": {Provider: "qwen-plan", OpenAIBaseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"},
	}}
	if err := c.validate(); err != nil {
		t.Errorf("qwen-plan provider_id rejected: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestValidate_AcceptsQwenPlanProviderID ./`
Expected: FAIL with `unknown provider_id "qwen-plan"`.

- [ ] **Step 3: Add qwen-plan to the allowlist** — edit `model-proxy/config.go`:

At `:623`, change the `known` map to include `"qwen-plan": true`:
```go
		known := map[string]bool{"aqp": true, "codex": true, "zhipu": true, "deepseek": true, "volcengine": true, "kimi-code": true, "static": true, "zcode": true, "qwen-plan": true}
```

At `:625`, append `qwen-plan` to the error string:
```go
			return fmt.Errorf("provider %q: unknown provider_id %q — valid: aqp, codex, zhipu, deepseek, volcengine, kimi-code, static, zcode, qwen-plan", name, p.Provider)
```

At `:618`, append `qwen-plan` to the hint:
```go
			return fmt.Errorf("provider %q: provider_id is empty — set `provider_id:` (e.g. zhipu, aqp, codex, deepseek, volcengine, qwen-plan)", name)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestValidate_AcceptsQwenPlanProviderID ./`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add model-proxy/config.go model-proxy/config_test.go
git commit -m "feat(config): register qwen-plan provider_id in the allowlist"
```

---

### Task 2: The provider (`provider/qwen_plan.go` + Usage display + tests)

**Files:**
- Create: `model-proxy/provider/qwen_plan.go`
- Modify: `model-proxy/provider/usage_display.go` (append `QwenPlanProvider.Usage()`)
- Test: `model-proxy/provider/qwen_plan_test.go` (create)

**Interfaces:**
- Consumes: `newApiKeyBaseBound(cfg, providerName)`, `fetchModelsBearer(cfg, auth)`, `listConfigModels([]string)`, display helpers `Dim/Bold/Blue/Gray/Cyan`, `captureStdoutProvider`/`contains` (test helpers in `provider` pkg).
- Produces: `Register("qwen-plan", …)` (picked up automatically by `provider.New`); `QwenPlanProvider` methods satisfying `provider.Provider`.

- [ ] **Step 1: Write the failing tests** — create `model-proxy/provider/qwen_plan_test.go`:

```go
package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newQwenPlanForTest(t *testing.T, cfg *Config) *QwenPlanProvider {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.BoundAPIKey == "" {
		cfg.BoundAPIKey = "sk-sp-testkey1234567890"
	}
	cfg.ProviderID = "qwen-plan"
	p, err := New(cfg, "qwen-plan")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*QwenPlanProvider)
}

func TestQwenPlan_AuthHeaders_DualWrite(t *testing.T) {
	p := newQwenPlanForTest(t, nil)
	req := httptest.NewRequest(http.MethodGet, "https://x/v1/models", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-sp-testkey1234567890" {
		t.Errorf("Authorization = %q, want Bearer <key>", got)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-sp-testkey1234567890" {
		t.Errorf("x-api-key = %q, want <key> (dual-write for the Anthropic gateway)", got)
	}
}

func TestQwenPlan_FetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"qwen3.7-max"},{"id":"glm-5.2"},{"id":"deepseek-v4-pro"}]}`))
	}))
	defer srv.Close()
	p := newQwenPlanForTest(t, &Config{OpenAIBaseURL: srv.URL})
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	want := []string{"qwen3.7-max", "glm-5.2", "deepseek-v4-pro"}
	if len(ids) != len(want) {
		t.Fatalf("FetchModels = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("FetchModels[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestQwenPlan_FetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"InvalidApiKey","message":"No API-key provided."}`))
	}))
	defer srv.Close()
	p := newQwenPlanForTest(t, &Config{OpenAIBaseURL: srv.URL})
	if _, err := p.FetchModels(); err == nil {
		t.Error("FetchModels on 401 returned nil error; want error so caller falls back to config models")
	}
}

func TestQwenPlan_Quota_BillingUnknownWithConsoleURL(t *testing.T) {
	p := newQwenPlanForTest(t, nil)
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown (no public Credits API)", s.Billing)
	}
	if len(s.Windows) != 0 {
		t.Errorf("Windows = %v, want none (unmeasured)", s.Windows)
	}
	joined := strings.Join(s.Notes, "\n")
	if !strings.Contains(joined, "platform.qianwenai.com/home/billing/subscription/token-plan-individual") {
		t.Errorf("Notes missing console URL:\n%s", joined)
	}
	if !strings.Contains(joined, "console") {
		t.Errorf("Notes missing console-only hint:\n%s", joined)
	}
}

func TestQwenPlan_Usage_PrintsConsoleURL(t *testing.T) {
	p := newQwenPlanForTest(t, &Config{Models: []string{"qwen3.7-max", "glm-5.2"}})
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "platform.qianwenai.com/home/billing/subscription/token-plan-individual") {
		t.Errorf("usage output missing console URL:\n%s", out)
	}
	if !contains(out, "qwen-plan") {
		t.Errorf("usage output missing provider name:\n%s", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test -run TestQwenPlan ./provider/`
Expected: FAIL (build error: `QwenPlanProvider` undefined).

- [ ] **Step 3: Implement the provider** — create `model-proxy/provider/qwen_plan.go`:

```go
package provider

import (
	"fmt"
	"net/http"
	"time"
)

// QwenPlanProvider implements 千问 AI Token Plan 个人版 (personal edition): a
// dual-protocol OpenAI + Anthropic byte-level passthrough on Alibaba Bailian's
// token-plan maas gateway, authenticated with one subscription API key (sk-sp-…).
//
// There is NO public Credits-usage API (personal-edition usage is console-only),
// so Quota() returns BillingUnknown carrying the console subscription URL in
// Notes; the 5h/7d window exhaustion surfaces reactively via the 429
// "Allocated quota exceeded" body, already classified as rlQuota by failclass.go
// → quota cooldown + failover.
type QwenPlanProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

// qwenPlanConsoleURL is the personal-edition subscription/usage page. Shown in
// the CLI `usage` output and the Web UI (QuotaSnapshot.Notes → app.js renders
// snap.Notes in the Accounts-tab provider detail) because the Credits numbers
// have no public API.
const qwenPlanConsoleURL = "https://platform.qianwenai.com/home/billing/subscription/token-plan-individual"

func init() {
	Register("qwen-plan", func(cfg *Config, providerName string) (Provider, error) {
		return &QwenPlanProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// AuthHeaders injects the key as BOTH Authorization: Bearer (OpenAI endpoint)
// and x-api-key (Anthropic endpoint), so one config serves both protocols
// (deepseek/zcode pattern). The OpenAI endpoint ignores x-api-key; this covers
// the Anthropic gateway whether it prefers Bearer or x-api-key.
//
// NOTE: login-time validation (validateKeyBearerGET) sends Bearer only — it
// probes the OpenAI /models endpoint, which is Bearer. Only the forward path
// (this method) dual-writes, because it must serve both protocols.
func (p *QwenPlanProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// RewriteRequest is a no-op: pure passthrough. The proxy selects the upstream
// base URL by protocol in proxy.forward (openai_base_url for OpenAI paths,
// anthropic_base_url for /v1/messages).
func (p *QwenPlanProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *QwenPlanProvider) Logout() error { return p.DeleteKey() }

// FetchModels lists models via the OpenAI-compatible /models endpoint. If the
// token-plan gateway hides the list for subscription plans (the Coding-Plan FAQ
// warns model lists may not be queryable), this errors and the caller (models
// refresh / usage display) falls back to the config models: list.
func (p *QwenPlanProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// Quota returns an unmeasured snapshot (no public Credits-usage API). The
// console subscription URL rides in Notes so both the CLI `usage` command and
// the Web UI (app.js renders snap.Notes) can link to the real 5h/7d numbers.
// BillingUnknown → the surplus scheduler ranks qwen-plan by priority (neutral);
// window exhaustion is handled reactively via 429 → rlQuota → cooldown/failover.
func (p *QwenPlanProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{
		Billing: BillingUnknown,
		Notes: []string{
			"Credits usage (5h/7d windows) is viewable only in the console",
			"Subscription details: " + qwenPlanConsoleURL,
		},
		AsOf: time.Now(),
	}, nil
}
```

- [ ] **Step 4: Add the Usage() display method** — append to `model-proxy/provider/usage_display.go` (after `VolcengineProvider.Usage`, before EOF):

```go
func (p *QwenPlanProvider) Usage() error {
	fmt.Printf("%s %s\n", Dim("Provider:  "), Bold(Blue(p.providerName)))
	fmt.Printf("%s Credits (5h + 7d windows; Lite/Standard/Pro — either hitting the cap pauses service)\n", Dim("Billing:    "))
	fmt.Printf("%s %s\n", Dim("Usage:      "), Gray("(console-only; no public Credits API)"))
	fmt.Printf("%s %s\n", Dim("Details:    "), Cyan(qwenPlanConsoleURL))
	listConfigModels(p.cfg.Models)
	return nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd model-proxy && go test -run TestQwenPlan ./provider/`
Expected: PASS (all 5 tests).

Also confirm the whole provider package still builds: `cd model-proxy && go vet ./provider/`

- [ ] **Step 6: Commit**

```bash
git add model-proxy/provider/qwen_plan.go model-proxy/provider/qwen_plan_test.go model-proxy/provider/usage_display.go
git commit -m "feat(provider): add qwen-plan (Token Plan personal edition) provider"
```

---

### Task 3: Generic login key-validation fallback (`apiKeyValidationURL`)

**Files:**
- Modify: `model-proxy/login.go` (add helper + wire 2 call sites)
- Test: `model-proxy/login_cmd_test.go` (append unit test)

**Interfaces:**
- Consumes: config `Provider` struct fields `UsageURL`, `OpenAIBaseURL`; existing `validateKeyBearerGET(url, key)`.
- Produces: `apiKeyValidationURL(prov Provider) string` — used by `runApiKeyLoginWithInput` (the "Validating…" gate) and `addApikeyAccount` (the actual GET).

- [ ] **Step 1: Write the failing test** — append to `model-proxy/login_cmd_test.go`:

```go
func TestApiKeyValidationURL_Fallback(t *testing.T) {
	cases := []struct {
		name string
		prov Provider
		want string
	}{
		{"usage_url wins", Provider{UsageURL: "https://x/balance", OpenAIBaseURL: "https://x/v1"}, "https://x/balance"},
		{"openai_base_url/models when no usage_url", Provider{UsageURL: "", OpenAIBaseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"}, "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/models"},
		{"trailing slash trimmed", Provider{UsageURL: "", OpenAIBaseURL: "https://x/v1/"}, "https://x/v1/models"},
		{"empty when neither set", Provider{UsageURL: "", OpenAIBaseURL: ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := apiKeyValidationURL(c.prov); got != c.want {
				t.Errorf("apiKeyValidationURL = %q, want %q", got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestApiKeyValidationURL_Fallback ./`
Expected: FAIL (build error: `apiKeyValidationURL` undefined).

- [ ] **Step 3: Implement the helper and wire it in** — edit `model-proxy/login.go`:

Add the helper just above `addApikeyAccount` (after the `runApiKeyLoginWithInput` func, ~line 172):

```go
// apiKeyValidationURL returns the endpoint used to validate an API key at login:
// prov.UsageURL when set, otherwise openai_base_url + "/models" (the natural
// Bearer-GET probe), or "" when neither is set (login skips validation). It is
// field-based (not provider_id-based): providers with a real usage API set
// usage_url (zhipu/deepseek/volcengine/kimi-code → unchanged); providers without
// one (qwen-plan) validate against openai_base_url/models. Shared by the
// "Validating…" message gate and addApikeyAccount's validateKeyBearerGET call.
func apiKeyValidationURL(prov Provider) string {
	if prov.UsageURL != "" {
		return prov.UsageURL
	}
	if prov.OpenAIBaseURL != "" {
		return strings.TrimRight(prov.OpenAIBaseURL, "/") + "/models"
	}
	return ""
}
```

Wire it into the "Validating…" gate in `runApiKeyLoginWithInput` (~line 137), changing:
```go
	if prov.UsageURL != "" {
		fmt.Fprintf(os.Stderr, "Validating API key...\n")
	}
```
to:
```go
	if apiKeyValidationURL(prov) != "" {
		fmt.Fprintf(os.Stderr, "Validating API key...\n")
	}
```

Wire it into `addApikeyAccount` (~line 191), changing:
```go
	if err := validateKeyBearerGET(prov.UsageURL, key); err != nil {
```
to:
```go
	if err := validateKeyBearerGET(apiKeyValidationURL(prov), key); err != nil {
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test -run TestApiKeyValidationURL_Fallback ./`
Expected: PASS.

Run the existing login tests to confirm no behavior change for usage_url providers:
Run: `cd model-proxy && go test -run 'TestAddApikeyAccount|TestValidateKeyBearerGET' ./`
Expected: PASS (zhipu usage_url path unchanged).

- [ ] **Step 5: Commit**

```bash
git add model-proxy/login.go model-proxy/login_cmd_test.go
git commit -m "feat(login): validate apikey via openai_base_url/models when usage_url is unset"
```

---

### Task 4: 429 classification regression guard (reactive path)

**Files:**
- Modify: `model-proxy/failclass_test.go` (append cases to the `classify429` table)

This is a regression guard — qwen-plan's two 429 strings must classify correctly with **no** code change (proves the reactive cooldown works).

- [ ] **Step 1: Add the regression cases** — in `model-proxy/failclass_test.go`, find the `classify429` table-driven test (cases like `{...insufficient_quota..., rlQuota}`) and append two rows:

```go
		{`{"code":"ArrearQuotaExceeded","message":"Allocated quota exceeded"}`, rlQuota},          // qwen-plan: 5h/7d window exhausted
		{`{"message":"Requests rate limit exceeded"}`, rlTransient},                              // qwen-plan: request-rate (concurrency)
```

(Place them among the existing `{body, want}` rows of the same table literal; keep the existing rows intact.)

- [ ] **Step 2: Run the test to verify it passes (no code change expected)**

Run: `cd model-proxy && go test -run TestClassify429 ./` (use the actual test name if different — find via `grep -n "func Test.*lassify429\|func Test.*429" failclass_test.go`)
Expected: PASS. The `"quota exceeded"` substring already matches `quotaExhaustedMarkers`; `"Requests rate limit exceeded"` matches nothing → `rlTransient`.

- [ ] **Step 3: Commit**

```bash
git add model-proxy/failclass_test.go
git commit -m "test(failclass): guard qwen-plan 429 classification (quota vs rate-limit)"
```

---

### Task 5: Config example + documentation

**Files:**
- Modify: `model-proxy/config.yaml` (add qwen-plan block)
- Modify: `model-proxy/defaults.go` (add qwen-plan block to `defaultConfigYAML`)
- Modify: `docs/backend-contracts.md` (qwen-plan section + naming-table row)
- Modify: `model-proxy/CLI.md` (login/usage dispatch)
- Modify: `model-proxy/README.md` (provider block + examples + credential table)
- Modify: `docs/decisions/intentional-behaviors.md` (intentional-behavior note)

- [ ] **Step 1: Add the config example** — in `model-proxy/config.yaml`, append after the `kimi-code:` block (before the `claude_mapping:` comment):

```yaml
  # Qianwen Token Plan 个人版 (千问 AI Token Plan personal edition). API key via
  # `login qwen-plan` (from the Token Plan console; key format sk-sp-…). Two
  # protocol bases: openai_base_url = OpenAI-compatible base, anthropic_base_url
  # = Anthropic-compatible base (no /v1; proxy keeps /v1/messages). No usage_url —
  # personal-edition Credits usage (5h/7d windows) is console-only (no public
  # API); `login qwen-plan` validates the key by probing openai_base_url/models.
  qwen-plan:
    provider_id: qwen-plan
    openai_base_url: https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
    anthropic_base_url: https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic
    models:
      - qwen3.8-max-preview
      - qwen3.7-max
      - qwen3.7-plus
      - qwen3.6-flash
      - glm-5.2
      - deepseek-v4-pro
```

- [ ] **Step 2: Mirror it in the `config init` template** — in `model-proxy/defaults.go`, add the same block (without the leading 2-space comment indentation quirks — match the surrounding `defaultConfigYAML` style) after the `kimi-code:` block (~line 102), before the `claude_mapping:` section.

- [ ] **Step 3: Verify both YAMLs still parse** — `cd model-proxy && go test -run TestConfig_ProviderBaseURLs ./` (loads config.yaml) and confirm `config init` output is valid YAML by eye. Expected: PASS.

- [ ] **Step 4: Add the backend contract** — in `docs/backend-contracts.md`:

Append a row to the token-file naming table (the `| provider_id | suffix | 文件名 |` table):
```
| qwen-plan | apikey | `~/.model-proxy/<name>_apikey.json`（单一 `{api_key}`）|
```

Add a new section (after the DeepSeek or Kimi-code section):
```markdown
## 千问 Token Plan 个人版契约（实测 + 探测）

- OpenAI base `https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1`（`/chat/completions`、`/responses`、`/models`）；Anthropic base `https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic`（**不带 /v1**，代理保留客户端 `/v1/messages`，同 DeepSeek/zhipu）。代理按协议字节级透传。
- 鉴权双写：每请求同时发 `Authorization: Bearer <sk-sp-key>` + `x-api-key: <key>`（同 DeepSeek/zcode，覆盖 Anthropic 网关偏好；OpenAI 端忽略 x-api-key）。key 为 `sk-sp-` 前缀的套餐专属 key，与通用 `sk-` key / `dashscope.aliyuncs.com` 域名**不可混用**。
- `/models` 为真实路由端点（无鉴权返回结构化 `{"code":"InvalidApiKey"}` 401，非 404）；但套餐可能不开放列表（Coding Plan FAQ 称「模型列表不支持通过接口查询」），故 `FetchModels` 失败时回退 config `models:`。
- **无公开用量/Credits 接口**：个人版 5h/7d Credits 用量仅在控制台「用量分析」页（`以控制台订阅页用量明细为准`）。`Quota()` 返回 `BillingUnknown` + Notes（含控制台 URL `https://platform.qianwenai.com/home/billing/subscription/token-plan-individual`），CLI `usage` 与 Web UI（`/api/status.quota` → app.js 渲染 `snap.Notes`）均展示该 URL。不轮询、不抓控制台（尊重平台「严禁 API 调用」条款，见 intentional-behaviors）。
- 429：`Allocated quota exceeded`（5h/7d 窗口耗尽）→ `failclass.go` 命中 `"quota exceeded"` → `rlQuota`（默认 1h 冷却或 body reset hint，上限 7d）→ 调度跳过并 failover；`Requests rate limit exceeded`（并发限频）→ `rlTransient`（60s）。
- 配额窗口：5h = 700/3000/12000 Credits，7d = 2500/10000/40000 Credits（Lite/Standard/Pro）；每次消耗同时计入两层，任一层触顶暂停。`login qwen-plan` 用 `openai_base_url/models`（Bearer GET，401/403 拒）验 key（无 usage_url，走 `apiKeyValidationURL` 兜底）。
```

- [ ] **Step 5: Update CLI.md** — in `model-proxy/CLI.md`, add `qwen-plan` to the login-dispatch provider list (~line 160, where zhipu/deepseek/kimi-code/volcengine/zcode are listed) and note it takes an API key via the default apikey login flow. Add `qwen-plan` to the usage dispatch (~line 282). No new command — it reuses `login <name>` / `usage <name>` / `logout <name>`.

- [ ] **Step 6: Update README.md** — in `model-proxy/README.md`:
  - Add a qwen-plan provider block near the deepseek/volcengine blocks (~line 408-425), summarizing: dual-protocol passthrough, `sk-sp-` key, console-only usage.
  - Add `login qwen-plan` / `usage qwen-plan` examples near the other login/usage examples (~line 117-134).
  - Add a row to the credential-storage table (~line 256-262): `qwen-plan | ~/.model-proxy/<name>_apikey.json | {api_key}`.

- [ ] **Step 7: Add the intentional-behavior note** — in `docs/decisions/intentional-behaviors.md`, append:

```markdown
## qwen-plan：用量仅控制台、不轮询（有意为之）

千问 Token Plan 个人版的 Credits 用量（5h/7d 窗口）**没有公开 API**，且平台条款「严禁 API 调用」明确禁止自动化/批量调用（仅允许 Claude Code/Cursor 等交互式工具）。model-proxy 作为交互式开发工具的转发代理，转发本身合规；但**有意不实现**控制台 cookie 抓取或后台配额轮询——`Quota()` 返回 `BillingUnknown`，仅把控制台订阅页 URL 附在 CLI `usage` 与 Web UI 的 Notes 里供人工查看。窗口耗尽由 429 `Allocated quota exceeded` 反应式触发冷却与 failover（`failclass.go` 已分类，无需新增代码）。
```

- [ ] **Step 8: Commit**

```bash
git add model-proxy/config.yaml model-proxy/defaults.go docs/backend-contracts.md model-proxy/CLI.md model-proxy/README.md docs/decisions/intentional-behaviors.md
git commit -m "docs(qwen-plan): config example + backend contract + CLI/README + intentional-behavior note"
```

---

### Task 6: Full verification

- [ ] **Step 1: Full test suite**

Run: `cd model-proxy && go test ./...`
Expected: PASS (all packages).

- [ ] **Step 2: Race check on new state paths**

Run: `cd model-proxy && go test -race -run 'TestQwenPlan|TestApiKeyValidationURL|TestClassify429' ./...`
Expected: PASS, no races.

- [ ] **Step 3: Coverage gate**

Run: `cd model-proxy && scripts/cover.sh`
Expected: ≥ 80% (the repo gate). Confirm `provider/qwen_plan.go` is covered by the new tests.

- [ ] **Step 4: Lint / whitespace**

Run: `git diff --check`
Expected: no whitespace errors.

- [ ] **Step 5: Sanity-build the daemon**

Run: `cd model-proxy && go build ./...`
Expected: builds cleanly.

- [ ] **Step 6: Confirm only qwen-plan files staged across the branch**

Run: `git log --oneline main..HEAD && git diff --stat main..HEAD`
Expected: only the files listed in this plan changed; the user's unrelated working-tree changes are NOT included in any commit.

---

## Self-Review

**1. Spec coverage** — spec §4 provider (Task 2) ✓; §5 allowlist (Task 1) + login fallback (Task 3) + no-op buildOne/logout/web/takeover (no task needed — verified default-path) ✓; §3 reactive cooldown (Task 4 regression guard) ✓; §6 config example (Task 5) ✓; §7 docs (Task 5) ✓; §8 testing (Tasks 1-4) ✓. Optional `quotaSourceLabel` polish (spec §5) omitted as non-essential; note for follow-up.

**2. Placeholder scan** — none. Every code step shows the full code; doc steps show the exact content/insertion point.

**3. Type consistency** — `apiKeyValidationURL(prov Provider)` matches the config `Provider` struct used in `runApiKeyLoginWithInput`/`addApikeyAccount`; `QwenPlanProvider` embeds `*ApiKeyBase`+`baseProbe` and uses `newApiKeyBaseBound`/`fetchModelsBearer`/`listConfigModels` exactly as defined; `Quota()` returns `*QuotaSnapshot` with `BillingUnknown` + `Notes` (fields from `provider.go`); test helpers `captureStdoutProvider`/`contains` exist in the `provider` test package.

## Notes for the executor
- The worktree has unrelated uncommitted changes — `git add` only the specific files per task; never `git add -A`.
- `captureStdoutProvider` and `contains` are existing helpers in `model-proxy/provider/*_test.go` (used by the aqp/zhipu usage tests) — reuse them, don't reimplement.
- If the Anthropic gateway 401s on dual-write during any future live-key test, simplifying `AuthHeaders` to Bearer-only is the documented fallback — but dual-write is the safe default for the untestable case.
