# Live codex `models refresh` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `model-proxy models refresh codex` return the codex backend's current live model list (e.g. `gpt-5.6-sol`) instead of the hardcoded `gpt-5.5`.

**Architecture:** `CodexProvider.FetchModels` (in `provider/`) does a real `GET <base>/models?client_version=<ver>`, authed via the existing `cfg.Auth.Inject`, parses the custom `{"models":[{slug,visibility}]}` shape, and returns `visibility=="list"` slugs. The `client_version` value is resolved in `main` (config → `codex --version` → `~/.codex/models_cache.json` → baked constant) and threaded into the provider via a new `provider.Config.ClientVersion` field set in `buildOne`.

**Tech Stack:** Go stdlib only (`net/http`, `httptest`, `os/exec`, `regexp`, `encoding/json`). No new dependencies.

## Global Constraints

- Go module lives in the `model-proxy/` subdirectory — **every `go` command runs from `model-proxy/`**.
- Tests are white-box (`package main` / `package provider`), stdlib `testing` + `httptest` only — **no testify**.
- Coverage baseline: **80% per package**, enforced by `scripts/cover.sh`.
- `gofmt -l .`, `go vet ./...`, `go test ./...` must all be clean before commit.
- Tests assert **exact values** (auth headers, filtered slugs, request URL) — never "non-empty".

---

## File Structure

- **Create** `model-proxy/codex_client_version.go` (package main) — `client_version` resolver + sources (`codex --version`, `~/.codex/models_cache.json`, constant). One responsibility: produce the version string.
- **Create** `model-proxy/codex_client_version_test.go` (package main) — resolver + semver-parse tests.
- **Create** `model-proxy/provider/codex_test.go` (package provider) — live `FetchModels` tests with a fake `Authenticator` + httptest.
- **Modify** `model-proxy/provider/codex.go` — rewrite `FetchModels` from hardcoded to live GET.
- **Modify** `model-proxy/provider/provider.go` — add `ClientVersion string` to `Config`.
- **Modify** `model-proxy/config.go` — add `ClientVersion string` to the YAML `Provider` struct.
- **Modify** `model-proxy/proxy.go` — set `pcfg.ClientVersion` in the `buildOne` codex case.
- **Modify** `CLAUDE.md`, `AGENTS.md`, `model-proxy/defaults.go` — document the live endpoint + optional config field.

---

## Task 1: `client_version` resolver (package main)

**Files:**
- Create: `model-proxy/codex_client_version.go`
- Test: `model-proxy/codex_client_version_test.go`

**Interfaces:**
- Produces: `resolveCodexClientVersion(configVal string, cliVer, cacheVer func() string) string`, `parseSemver(s string) string`, `codexCLIVersion() string`, `codexCacheVersion() string`, const `defaultCodexClientVersion = "0.144.1"`. Consumed by Task 3's `buildOne` wiring.

- [ ] **Step 1: Write the failing tests**

Create `model-proxy/codex_client_version_test.go`:

```go
package main

import "testing"

func TestResolveCodexClientVersion(t *testing.T) {
	cli := func() string { return "0.144.1" }
	cache := func() string { return "0.130.0" }

	// config value wins.
	if got := resolveCodexClientVersion("0.200.0", cli, cache); got != "0.200.0" {
		t.Errorf("config should win: got %q want 0.200.0", got)
	}
	// config empty → cli.
	if got := resolveCodexClientVersion("", cli, cache); got != "0.144.1" {
		t.Errorf("cli fallback: got %q want 0.144.1", got)
	}
	// config + cli empty → cache.
	if got := resolveCodexClientVersion("", func() string { return "" }, cache); got != "0.130.0" {
		t.Errorf("cache fallback: got %q want 0.130.0", got)
	}
	// all empty → constant.
	if got := resolveCodexClientVersion("", func() string { return "" }, func() string { return "" }); got != defaultCodexClientVersion {
		t.Errorf("constant fallback: got %q want %q", got, defaultCodexClientVersion)
	}
	// whitespace-only config is treated as empty.
	if got := resolveCodexClientVersion("  ", cli, cache); got != "0.144.1" {
		t.Errorf("whitespace config should fall through: got %q", got)
	}
}

func TestParseSemver(t *testing.T) {
	cases := []struct{ in, want string }{
		{"codex 0.144.1", "0.144.1"},
		{"codex 0.144.1\n", "0.144.1"},
		{"codex 1.0.0 (abcd123) ", "1.0.0"},
		{"no version here", ""},
	}
	for _, c := range cases {
		if got := parseSemver(c.in); got != c.want {
			t.Errorf("parseSemver(%q): got %q want %q", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test -run 'TestResolveCodexClientVersion|TestParseSemver' .`
Expected: FAIL / build error (`resolveCodexClientVersion` / `parseSemver` / `defaultCodexClientVersion` undefined).

- [ ] **Step 3: Write the implementation**

Create `model-proxy/codex_client_version.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// defaultCodexClientVersion is the last-resort client_version sent to the codex
// /models endpoint when neither config, the codex CLI, nor the codex models
// cache yields a version. Bump manually; auto-detection normally wins.
const defaultCodexClientVersion = "0.144.1"

var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// resolveCodexClientVersion returns the first non-empty (trimmed) version from
// configVal, cliVer, cacheVer (in that order); if all are empty it falls back
// to defaultCodexClientVersion. cliVer/cacheVer are func params so tests can
// inject fakes without spawning processes or touching the filesystem.
func resolveCodexClientVersion(configVal string, cliVer, cacheVer func() string) string {
	for _, src := range []func() string{func() string { return configVal }, cliVer, cacheVer} {
		if v := strings.TrimSpace(src()); v != "" {
			return v
		}
	}
	return defaultCodexClientVersion
}

// parseSemver extracts the first MAJOR.MINOR.PATCH token from s (e.g. the
// output of `codex --version`, "codex 0.144.1"). Empty if none found.
func parseSemver(s string) string {
	if m := semverRe.FindString(s); m != "" {
		return m
	}
	return ""
}

// codexCLIVersion runs `codex --version` and returns its MAJOR.MINOR.PATCH
// version, or "" if the codex CLI is absent or unparsable.
func codexCLIVersion() string {
	out, err := exec.Command("codex", "--version").Output()
	if err != nil {
		return ""
	}
	return parseSemver(string(out))
}

// codexCacheVersion reads the client_version the codex CLI persisted in its
// models cache, or "" if the file is absent/unreadable.
func codexCacheVersion() string {
	data, err := os.ReadFile(codexModelsCachePath())
	if err != nil {
		return ""
	}
	var v struct {
		ClientVersion string `json:"client_version"`
	}
	if json.Unmarshal(data, &v) != nil {
		return ""
	}
	return v.ClientVersion
}

// codexHome returns the codex CLI config dir: $CODEX_HOME if set, else ~/.codex.
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func codexModelsCachePath() string {
	return filepath.Join(codexHome(), "models_cache.json")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestResolveCodexClientVersion|TestParseSemver' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add codex_client_version.go codex_client_version_test.go
git commit -m "feat(codex): add client_version resolver (config > cli > cache > const)"
```

---

## Task 2: Live `FetchModels` (package provider)

**Files:**
- Modify: `model-proxy/provider/codex.go` (replace `FetchModels` at lines 39-44; add imports `fmt`, `io`, `strings`)
- Modify: `model-proxy/provider/provider.go` (add `ClientVersion string` to `Config` after line 137)
- Test: `model-proxy/provider/codex_test.go`

**Interfaces:**
- Consumes: `provider.Config.ClientVersion` (added here), `provider.Config.Auth.Inject`, the package-level `truncateStr` (already in `fetch_models.go`).
- Produces: `(*CodexProvider).FetchModels() ([]string, error)` doing a live GET — the same signature `models refresh` already calls.

- [ ] **Step 1: Add the `ClientVersion` field to `provider.Config`**

In `model-proxy/provider/provider.go`, find the `Config` struct and add the field right after `Models map[string]any` (currently line 137):

```go
	Models        map[string]any

	// ClientVersion is the codex /models client_version query param, resolved
	// by the main package (config > codex CLI > ~/.codex cache > constant) and
	// passed in via buildOne. Unused by other providers.
	ClientVersion string
```

- [ ] **Step 2: Write the failing tests**

Create `model-proxy/provider/codex_test.go`:

```go
package provider

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// fakeAuth is a minimal Authenticator for codex FetchModels tests — no file IO.
type fakeAuth struct{ token, acct string }

func (f fakeAuth) Inject(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("originator", "codex_cli_rs")
	if f.acct != "" {
		req.Header.Set("ChatGPT-Account-Id", f.acct)
	}
	return nil
}
func (fakeAuth) Refresh() error { return nil }

func TestCodexFetchModels_LiveQuery(t *testing.T) {
	var (
		gotURL                            string
		gotAuth, gotOriginator, gotAcct   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		gotOriginator = r.Header.Get("originator")
		gotAcct = r.Header.Get("ChatGPT-Account-Id")
		w.Write([]byte(`{"models":[
			{"slug":"gpt-5.6-sol","visibility":"list"},
			{"slug":"gpt-5.5","visibility":"list"},
			{"slug":"codex-auto-review","visibility":"hide"},
			{"slug":"gpt-5.4","visibility":"none"}
		]}`))
	}))
	defer srv.Close()

	p := &CodexProvider{cfg: &Config{
		OpenAIBaseURL: srv.URL + "/backend-api/codex",
		ClientVersion: "0.144.1",
		Auth:          fakeAuth{token: "tok-abc", acct: "acct-123"},
	}}
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	want := []string{"gpt-5.6-sol", "gpt-5.5"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("ids: got %v, want %v", ids, want)
	}
	if gotURL != "/backend-api/codex/models?client_version=0.144.1" {
		t.Errorf("URL: got %q, want /backend-api/codex/models?client_version=0.144.1", gotURL)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization: got %q, want Bearer tok-abc", gotAuth)
	}
	if gotOriginator != "codex_cli_rs" {
		t.Errorf("originator: got %q, want codex_cli_rs", gotOriginator)
	}
	if gotAcct != "acct-123" {
		t.Errorf("ChatGPT-Account-Id: got %q, want acct-123", gotAcct)
	}
}

func TestCodexFetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{
		OpenAIBaseURL: srv.URL + "/codex",
		ClientVersion: "0.144.1",
		Auth:          fakeAuth{},
	}}
	_, err := p.FetchModels()
	if err == nil {
		t.Fatal("expected error on HTTP 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status 403, got %q", err.Error())
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd model-proxy && go test -run 'TestCodexFetchModels' ./provider/`
Expected: FAIL (`FetchModels` still returns hardcoded `["gpt-5.5"]`; the live-query test gets the wrong list / no HTTP call captured).

- [ ] **Step 4: Rewrite `FetchModels` to do the live GET**

In `model-proxy/provider/codex.go`, first fix the imports (currently `encoding/json`, `net/http`, `time`) — add `fmt`, `io`, `strings`:

```go
import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)
```

Then replace the `FetchModels` method AND its comment (currently lines 39-44) with:

```go
// FetchModels queries the codex backend's /models endpoint and returns the
// slugs with visibility "list". The endpoint is gated on a client_version
// query param (the backend uses it to decide which models to expose — a stale
// version hides newer models) and returns a custom {"models":[{slug,visibility,
// ...}]} shape rather than OpenAI's {data:[]}, so it cannot reuse
// fetchModelsBearer. client_version is resolved by the main package (config >
// codex CLI > ~/.codex cache > baked constant) and passed in via cfg.ClientVersion.
func (p *CodexProvider) FetchModels() ([]string, error) {
	url := strings.TrimRight(p.cfg.OpenAIBaseURL, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("client_version", p.cfg.ClientVersion)
	req.URL.RawQuery = q.Encode()
	if err := p.cfg.Auth.Inject(req); err != nil {
		return nil, fmt.Errorf("codex models auth: %w", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch codex models: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch codex models: HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}
	var v struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("parse codex models: %w", err)
	}
	ids := make([]string, 0, len(v.Models))
	for _, m := range v.Models {
		if m.Visibility == "list" {
			ids = append(ids, m.Slug)
		}
	}
	return ids, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestCodexFetchModels' ./provider/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd model-proxy
git add provider/codex.go provider/codex_test.go provider/provider.go
git commit -m "feat(codex): query live /models instead of hardcoding gpt-5.5"
```

---

## Task 3: Wire the resolver into `buildOne` + YAML field

**Files:**
- Modify: `model-proxy/config.go` (add `ClientVersion` to the `Provider` struct, after `AqpMintURL` at line 95)
- Modify: `model-proxy/proxy.go` (set `pcfg.ClientVersion` in the `buildOne` codex case, line 204)

**Interfaces:**
- Consumes: `resolveCodexClientVersion`, `codexCLIVersion`, `codexCacheVersion` (Task 1); `provider.Config.ClientVersion` (Task 2); the YAML `Provider.ClientVersion` field added here.
- Produces: an end-to-end path so `models refresh codex` → `FetchModels` carries a resolved `client_version`.

- [ ] **Step 1: Add the YAML field to the main `Provider` struct**

In `model-proxy/config.go`, find the `AqpMintURL` line (line 95) and add `ClientVersion` directly after it (gofmt will realign the column block):

```go
	AqpMintURL       string                   `yaml:"aqp_mint_url"` // aqp only
	ClientVersion    string                   `yaml:"client_version,omitempty"`
```

- [ ] **Step 2: Set `pcfg.ClientVersion` in the codex case of `buildOne`**

In `model-proxy/proxy.go`, find the `case "codex":` block (line 204) and add the `ClientVersion` line as its first statement:

```go
	case "codex":
		pcfg.ClientVersion = resolveCodexClientVersion(prov.ClientVersion, codexCLIVersion, codexCacheVersion)
		pcfg.LoginFn = func() error { return runCodexLogin(cfg) }
		pcfg.LogoutFn = func() error { return clearCodexAuth(cfg) }
		pcfg.UsageFn = func() (any, error) { return showCodexUsageData(cfg, prov) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCodexQuota(cfg, prov) }
```

- [ ] **Step 3: Verify the whole module builds, lints, tests, and is formatted clean**

Run, all must succeed with no output for the lint/format checks:

```bash
cd model-proxy
go build ./...
go vet ./...
gofmt -l .
go test ./...
```
Expected: build OK; `go vet` clean; `gofmt -l .` prints nothing; all tests PASS.

- [ ] **Step 4: Verify coverage stays ≥80% per package**

Run: `cd model-proxy && ../scripts/cover.sh --no-enforce`
Expected: both `main` and `provider` packages report ≥80% (the new code in Tasks 1–2 is fully covered by the new tests). Note the printed percentages; if either dropped, add assertions before proceeding. (Do not commit `cov.out` / `coverage.html` — they are gitignored artifacts.)

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add config.go proxy.go
git commit -m "feat(codex): wire client_version resolution into buildOne + config field"
```

---

## Task 4: Documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `AGENTS.md`
- Modify: `model-proxy/defaults.go` (add an optional commented `client_version` under the codex provider for discoverability)

**No test cycle** — docs. The verification is reading the rendered text.

- [ ] **Step 1: Update the codex bullet in `CLAUDE.md`**

Find this exact line in the "Provider abstraction (`provider/provider.go`)" section:

```
- `codex` — OAuth device flow; `RewriteRequest` injects `store:false`; `FetchModels` hardcodes `["gpt-5.5"]` (the codex backend's `/models` needs a `client_version` param, not worth querying).
```

Replace it with:

```
- `codex` — OAuth device flow; `RewriteRequest` injects `store:false`; `FetchModels` queries the backend's `/models?client_version=<ver>` (custom `{"models":[{slug,visibility,...}]}` shape, filtered to `visibility=="list"`, 10s timeout). `client_version` is resolved in `main` (config `providers.codex.client_version` > `codex --version` > `~/.codex/models_cache.json` > baked constant) — the backend gates newer models behind a recent-enough version, so a stale value silently shrinks the list.
```

- [ ] **Step 2: Add the `models` row to the codex contract table in `AGENTS.md`**

Open `AGENTS.md`, find the `### codex 后端契约（实测）` table (around lines 346-354), and add this row (keep the existing rows; insert the `models` row after the `usage` row, before the `originator` header row):

```
| models | GET | codex OAuth Bearer | `/backend-api/codex/models?client_version=<ver>`，返回 `{"models":[{slug,visibility,...}]}`，仅取 `visibility=="list"`；`client_version` 决定可见模型（过低则新模型不返回） |
```

- [ ] **Step 3: Add a gotcha about `client_version` gating in `AGENTS.md`**

In the `AGENTS.md` pitfalls section (`踩过的坑`), add a new numbered entry alongside the existing codex entries:

```
- **codex /models 的 client_version 闸门**：`/backend-api/codex/models` 必须带 `client_version` 查询参数；后端据此决定返回哪些模型，版本过旧则新模型（如 gpt-5.6）不会出现。model-proxy 的解析顺序：config `client_version` → `codex --version` → `~/.codex/models_cache.json` → 内置常量。
```

- [ ] **Step 4: Add an optional commented `client_version` to the codex default config**

In `model-proxy/defaults.go`, find the `codex:` provider block and add a commented line under it (so operators discover the override). Locate the block containing `provider_id: codex` and add immediately after the `openai_base_url` line:

```yaml
    # client_version: "0.144.1"   # optional; auto-detected from codex CLI if omitted
```

- [ ] **Step 5: Commit**

```bash
cd model-proxy
git add ../CLAUDE.md ../AGENTS.md defaults.go
git commit -m "docs(codex): document live /models endpoint + client_version config"
```

---

## Self-Review (completed during planning)

- **Spec coverage:** §1 resolver → Task 1. §2 live fetch → Task 2 (+ `Config.ClientVersion`). §3 config field → Task 3 Step 1. §4 error-on-failure → Task 2 (`ErrorOnNon200` test asserts an error, no fallback). §5 testing → Tasks 1 & 2 tests. Out-of-scope items (ETag, auto-route-edit, UA) intentionally absent. Docs → Task 4.
- **Placeholders:** none — every code step contains real Go.
- **Type consistency:** `resolveCodexClientVersion(string, func() string, func() string) string` matches across Task 1 (def) and Task 3 (call). `provider.Config.ClientVersion` matches across Task 2 (read) and Task 3 (write). `prov.ClientVersion` (Task 3) matches the YAML field added in Task 3 Step 1.
