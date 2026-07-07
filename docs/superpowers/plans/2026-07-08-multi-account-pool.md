# Multi-Account Credential Pool — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one config provider (e.g. `zhipu`) hold multiple accounts added via repeated `login`, and actively spread requests across them (round-robin) while keeping per-account quota/circuit/429 isolation.

**Architecture:** A credential pool per provider is unrolled at `buildProviders` time into N virtual providers (`name#<accountID>`, or plain `name` when single) that share one config entry but bind distinct credentials. The existing schedule/health/quota/sticky machinery is unchanged — virtuals are ordinary provider names to it. A new `strategy: spread` adds a localized round-robin branch in `decideOrder`; default `sticky` needs no scheduling change.

**Tech Stack:** Go 1.x, module in `model-proxy/`, stdlib `net/http` + `gopkg.in/yaml.v3`. Tests white-box (`package main` / `package provider`), stdlib `testing` + `httptest` only.

## Global Constraints

- Run all `go` commands from `model-proxy/`. `go test ./...` (~30s), `go test -race ./...`, `go vet ./...`, `gofmt -l .` must all stay clean.
- **80% coverage baseline** enforced by `scripts/cover.sh` — keep it green.
- **No testify.** Stdlib `testing` + `httptest` only.
- **Exact-value assertions** (the repo's green-signal guard): assert exact `Bearer <token>` for auth, exact account IDs, exact round-robin distribution counts — never "non-empty" or count-only.
- **Credentials never in config.yaml**; stored under `~/.model-proxy/<name>_<suffix>.json`. Pool file is `~/.model-proxy/<name>_apikeys.json` (plural).
- **Lock ordering:** `healthMu` → `quotaMu` (never reverse). New `spreadCtr` lives under `healthMu`.
- **Logging hygiene:** mask secrets with `mask()`; never log full keys/cookies.
- Single-account behavior is byte-for-byte unchanged (pool size 1 → virtual id == parent name).

## File Structure

**Create:**
- `model-proxy/pool.go` — pool struct, load/save, singular-file fallback migration, `accountIDFor`, `accountCred`. Pure data layer; no scheduling.
- `model-proxy/pool_test.go` — round-trip, migration, id-extraction tests.

**Modify:**
- `model-proxy/provider/apikey.go` — `NewApiKeyBaseWithKey` (in-memory bound key; skips file read).
- `model-proxy/auth.go` — `newAuthProvider` gains a `*accountCred` binding param; `newApiKeyProviderWithKey`.
- `model-proxy/config.go` — `Strategy` field on `Provider` + validation.
- `model-proxy/proxy.go` — `buildProviders` unrolling + `poolIndex`/`parentOf`/`spreadParents`; `Proxy.spreadCtr` + `expandedRoutes`; `decideOrder` spread branch + `commit` param; `forward`/`scheduleStatus` use expanded routes.
- `model-proxy/provider_wire.go` — thread `*accountCred` through `showZhipuUsageData`/`showDeepseekUsageData`/`showVolcengineUsageData`.
- `model-proxy/main.go` — `showGenericUsage`/`fetchZhipuQuota`/`fetchDeepseekQuota` take `*accountCred`; `cmdUsage` + `cmdLogout` pool-aware.
- `model-proxy/login.go` — `runApiKeyLogin` pool-aware (dedup, `--label`, `--replace`, reload signal).
- `model-proxy/models.go` — `models refresh` runs once per provider (first account).
- `model-proxy/cmd_schedule.go`/`main.go` (`cmdSchedule`, `cmdDoctor`) — group virtuals under parent for display.

---

## Task 1: Pool storage + accountID + migration

**Files:**
- Create: `model-proxy/pool.go`
- Create: `model-proxy/pool_test.go`

**Interfaces:**
- Produces: `accountCred`, `poolAccount`, `credentialPool`, `poolPath(name)`, `singularPoolPath(name)`, `loadPool(name, providerID) (credentialPool, error)`, `savePool(name, credentialPool) error`, `accountIDFor(providerID, cred accountCred) string`. Later tasks import these.

- [ ] **Step 1: Write the failing test** (`pool_test.go`)

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func setHome(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700)
	t.Setenv("HOME", dir)
}

func TestAccountIDFor(t *testing.T) {
	z := accountIDFor("zhipu", accountCred{APIKey: "sk-abc"})
	z2 := accountIDFor("zhipu", accountCred{APIKey: "sk-abc"})
	z3 := accountIDFor("zhipu", accountCred{APIKey: "sk-other"})
	if z != z2 {
		t.Fatalf("same key must yield same id: %q vs %q", z, z2)
	}
	if z == z3 {
		t.Fatalf("different keys must yield different ids")
	}
	if len(z) != 16 {
		t.Fatalf("zhipu id len = %d, want 16", len(z))
	}
	// volcengine keys by access_key (account-level), not api_key
	a := accountIDFor("volcengine", accountCred{APIKey: "k1", AccessKey: "AK9"})
	b := accountIDFor("volcengine", accountCred{APIKey: "k2", AccessKey: "AK9"})
	if a != b {
		t.Fatalf("volcengine same access_key must yield same id: %q vs %q", a, b)
	}
	if a != "AK9" {
		t.Fatalf("volcengine id = %q, want AK9", a)
	}
	// access_key empty → fall back to key hash
	c := accountIDFor("volcengine", accountCred{APIKey: "k1"})
	if c == "" || len(c) != 16 {
		t.Fatalf("volcengine fallback id = %q", c)
	}
}

func TestSaveLoadPoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	in := credentialPool{Version: 1, Accounts: []poolAccount{
		{ID: "id1", Label: "home", APIKey: "k1", AddedAt: "2026-07-08T00:00:00Z"},
		{ID: "id2", Label: "team", APIKey: "k2", AddedAt: "2026-07-08T00:00:00Z"},
	}}
	if err := savePool("zhipu", in); err != nil {
		t.Fatal(err)
	}
	out, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("len = %d, want 2", len(out.Accounts))
	}
	if out.Accounts[0].ID != "id1" || out.Accounts[0].APIKey != "k1" || out.Accounts[0].Label != "home" {
		t.Fatalf("account0 = %+v", out.Accounts[0])
	}
}

func TestLoadPoolSingularFallback(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	// Legacy single-key file, no plural pool.
	singular := filepath.Join(dir, ".model-proxy", "zhipu_apikey.json")
	os.WriteFile(singular, []byte(`{"api_key":"legacy-key"}`), 0o600)

	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("fallback len = %d, want 1", len(pool.Accounts))
	}
	if pool.Accounts[0].APIKey != "legacy-key" {
		t.Fatalf("fallback key = %q", pool.Accounts[0].APIKey)
	}
	// id derived from the key.
	if pool.Accounts[0].ID != accountIDFor("zhipu", accountCred{APIKey: "legacy-key"}) {
		t.Fatalf("fallback id not derived from key")
	}
}

func TestLoadPoolEmpty(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("missing pool should be empty, not error: %v", err)
	}
	if len(pool.Accounts) != 0 {
		t.Fatalf("want 0 accounts, got %d", len(pool.Accounts))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run 'TestAccountIDFor|TestSaveLoadPool|TestLoadPool' .`
Expected: FAIL — `pool.go` symbols undefined.

- [ ] **Step 3: Write the implementation** (`pool.go`)

```go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// accountCred is one account's raw credentials, independent of storage format.
// APIKey is used for Bearer/x-api-key auth; AccessKey/SecretKey are volcengine's
// V4-signing pair (used by GetAFPUsage quota).
type accountCred struct {
	APIKey    string
	AccessKey string
	SecretKey string
}

type poolAccount struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key,omitempty"` // volcengine
	SecretKey string `json:"secret_key,omitempty"` // volcengine
	AddedAt   string `json:"added_at"`
}

type credentialPool struct {
	Version  int           `json:"version"`
	Accounts []poolAccount `json:"accounts"`
}

func (a poolAccount) cred() accountCred {
	return accountCred{APIKey: a.APIKey, AccessKey: a.AccessKey, SecretKey: a.SecretKey}
}

func poolPath(name string) string {
	return filepath.Join(homeDir(), ".model-proxy", name+"_apikeys.json")
}
func singularPoolPath(name string) string {
	return filepath.Join(homeDir(), ".model-proxy", name+"_apikey.json")
}

// loadPool reads the plural pool file; if absent, wraps the legacy singular
// <name>_apikey.json as a read-only 1-entry pool. A missing pool is empty (not
// an error) — the caller treats "no accounts" as "not logged in".
func loadPool(name, providerID string) (credentialPool, error) {
	data, err := os.ReadFile(poolPath(name))
	if err == nil {
		var p credentialPool
		if err := json.Unmarshal(data, &p); err != nil {
			return credentialPool{}, fmt.Errorf("parse %s: %w", poolPath(name), err)
		}
		return p, nil
	}
	if !os.IsNotExist(err) {
		return credentialPool{}, err
	}
	// Fall back to legacy singular file.
	sdata, serr := os.ReadFile(singularPoolPath(name))
	if serr != nil {
		if os.IsNotExist(serr) {
			return credentialPool{}, nil
		}
		return credentialPool{}, serr
	}
	var v struct {
		APIKey    string `json:"api_key"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(sdata, &v); err != nil {
		return credentialPool{}, fmt.Errorf("parse %s: %w", singularPoolPath(name), err)
	}
	cred := accountCred{APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey}
	return credentialPool{
		Version: 1,
		Accounts: []poolAccount{{
			ID:     accountIDFor(providerID, cred),
			Label:  providerID,
			APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey,
		}},
	}, nil
}

func savePool(name string, p credentialPool) error {
	path := poolPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if p.Version == 0 {
		p.Version = 1
	}
	sort.SliceStable(p.Accounts, func(i, j int) bool { return p.Accounts[i].ID < p.Accounts[j].ID })
	data, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

// accountIDFor returns a stable per-account identifier for dedup + virtual-id
// suffixing. volcengine keys by AccessKey (account-level); other apikey
// providers hash the APIKey (key-level). volcengine with no AccessKey falls
// back to the key hash.
func accountIDFor(providerID string, c accountCred) string {
	if providerID == "volcengine" && c.AccessKey != "" {
		return c.AccessKey
	}
	sum := sha256.Sum256([]byte(c.APIKey))
	return hex.EncodeToString(sum[:])[:16]
}

// nowTS is a helper for AddedAt timestamps (tests can't call time.Now directly
// in some harnesses; kept simple here).
func nowTS() string { return time.Now().UTC().Format(time.RFC3339) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run 'TestAccountIDFor|TestSaveLoadPool|TestLoadPool' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add model-proxy/pool.go model-proxy/pool_test.go
git commit -m "feat(pool): credential pool storage, accountID, singular fallback"
```

---

## Task 2: Config `strategy` field + validation

**Files:**
- Modify: `model-proxy/config.go` (add field to `Provider`, validate in `validate()`)
- Modify: `model-proxy/config_test.go`

**Interfaces:**
- Produces: `Provider.Strategy string` (`""`/`"sticky"`/`"spread"`). Consumed by Task 6's `isSpreadRoute`.

- [ ] **Step 1: Write the failing test** (append to `config_test.go`)

```go
func TestProviderStrategyValidate(t *testing.T) {
	cases := []struct {
		strat string
		ok    bool
	}{
		{"", true}, {"sticky", true}, {"spread", true},
		{"round-robin", false}, {"ROUND_ROBIN", false},
	}
	for _, tc := range cases {
		cfg := &Config{
			Listen: "127.0.0.1:1",
			Providers: map[string]Provider{
				"z": {OpenAIBaseURL: "https://x", Provider: "zhipu", Strategy: tc.strat},
			},
		}
		err := cfg.validate()
		if tc.ok && err != nil {
			t.Fatalf("strat %q should be valid: %v", tc.strat, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("strat %q should be rejected", tc.strat)
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `Provider.Strategy` undefined.

- [ ] **Step 3: Implement** — add field to the `Provider` struct in `config.go` (after `Billing`):

```go
	// Strategy is "sticky" (default; park on one account + failover) or "spread"
	// (round-robin across the credential pool's accounts). Only affects providers
	// with a multi-account pool.
	Strategy string `yaml:"strategy"`
```

Add to `validate()`, inside the `for name, p := range c.Providers` loop (after the billing check):

```go
		if p.Strategy != "" && p.Strategy != "sticky" && p.Strategy != "spread" {
			return fmt.Errorf("provider %q: strategy %q invalid — use \"sticky\" or \"spread\"", name, p.Strategy)
		}
```

- [ ] **Step 4: Run, expect PASS**: `cd model-proxy && go test -run TestProviderStrategyValidate .`

- [ ] **Step 5: Commit**

```bash
git add model-proxy/config.go model-proxy/config_test.go
git commit -m "feat(config): provider strategy field (sticky|spread)"
```

---

## Task 3: Credential binding — `NewApiKeyBaseWithKey` + thread `*accountCred`

**Files:**
- Modify: `model-proxy/provider/apikey.go`
- Modify: `model-proxy/auth.go` (`newAuthProvider` signature + `newApiKeyProviderWithKey`)
- Modify: `model-proxy/provider_wire.go` (usage wrappers take `*accountCred`)
- Modify: `model-proxy/main.go` (`showGenericUsage`, `fetchZhipuQuota`, `fetchDeepseekQuota` take `*accountCred`)
- Test: `model-proxy/provider/apikey_test.go` (extend), `model-proxy/auth_test.go` (extend)

**Interfaces:**
- Produces: `provider.NewApiKeyBaseWithKey(name, key)`, `newAuthProvider(authName, provName, cfg, cred *accountCred)`, and `*accountCred` params on `showGenericUsage`/`fetchZhipuQuota`/`fetchDeepseekQuota` + the `provider_wire.go` wrappers. nil `cred` = read file (backward compat).

- [ ] **Step 1: Write the failing test** (`provider/apikey_test.go`, append)

```go
package provider

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestApiKeyBaseWithKeyInjectsBoundKey(t *testing.T) {
	b := NewApiKeyBaseWithKey("zhipu", "BOUND-TOKEN")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://x/v1/m", nil)
	// AuthHeaders injects on the outbound request.
	if err := b.AuthHeaders(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer BOUND-TOKEN" {
		t.Fatalf("Authorization = %q, want Bearer BOUND-TOKEN", got)
	}
	if req.Header.Get("x-api-key") != "" {
		t.Fatalf("x-api-key should be deleted, got %q", req.Header.Get("x-api-key"))
	}
}

func TestApiKeyBaseWithKeyIgnoresFile(t *testing.T) {
	b := NewApiKeyBaseWithKey("zhipu", "BOUND")
	// LoadKey must return the bound key even though no file exists.
	got, err := b.LoadKey()
	if err != nil || got != "BOUND" {
		t.Fatalf("LoadKey = %q,%v want BOUND,nil", got, err)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `NewApiKeyBaseWithKey` undefined.

- [ ] **Step 3: Implement** `provider/apikey.go` — add a `bound` marker and the constructor:

```go
type ApiKeyBase struct {
	authFile string
	bound    bool // true → use cached key, never touch the file

	mu     sync.Mutex
	cached string
}

// NewApiKeyBaseWithKey binds an in-memory key (used when a provider is unrolled
// from a credential-pool entry). File reads/writes are skipped.
func NewApiKeyBaseWithKey(providerName, key string) *ApiKeyBase {
	return &ApiKeyBase{bound: true, cached: key}
}
```

In `LoadKey`, short-circuit when bound:

```go
func (b *ApiKeyBase) LoadKey() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bound || b.cached != "" {
		return b.cached, nil
	}
	// ...existing file-read path...
}
```

Make `SaveKey`/`DeleteKey` no-op (return nil) when `b.bound` — a pool-bound instance is never the source of truth for the file:

```go
func (b *ApiKeyBase) SaveKey(key string) error {
	if b.bound {
		return nil
	}
	// ...existing...
}
func (b *ApiKeyBase) DeleteKey() error {
	if b.bound {
		return nil
	}
	// ...existing...
}
```

- [ ] **Step 4: Run provider test, expect PASS**: `cd model-proxy && go test ./provider/ -run ApiKeyBaseWithKey`

- [ ] **Step 5: Thread `*accountCred` through auth.go** — change `newAuthProvider`:

```go
func newAuthProvider(authName, provName string, cfg *Config, cred *accountCred) AuthProvider {
	prov := cfg.Providers[provName]
	switch authName {
	case "aqp":
		return newAqpKeyProvider(prov.AqpMintURL, authFilePath(provName, "oauth_auth"))
	case "codex":
		return newCodexOAuthProvider(authFilePath(provName, "oauth_auth"))
	case "apikey":
		return newApiKeyProvider(authFilePath(provName, "apikey"))
	case "zhipu", "deepseek", "volcengine":
		path := authFilePath(provName, "apikey")
		if cred != nil && cred.APIKey != "" {
			return newApiKeyProviderWithKey(path, cred.APIKey)
		}
		return newApiKeyProvider(path)
	case "static":
		return &StaticProvider{key: ""}
	default:
		return &StaticProvider{key: ""}
	}
}
```

Add `newApiKeyProviderWithKey` next to `newApiKeyProvider`:

```go
// newApiKeyProviderWithKey builds an apikey AuthProvider bound to an in-memory
// key (credential-pool entry) instead of reading the auth file.
func newApiKeyProviderWithKey(path, key string) AuthProvider {
	b := NewApiKeyBaseWithKey(filepath.Base(path), key)
	b.authFile = path
	return &apiKeyProvider{base: b}
}
```

(Adjust to match the existing `newApiKeyProvider`/`apiKeyProvider` shape in `auth.go` — the key point is it wraps an `ApiKeyBase` produced by `NewApiKeyBaseWithKey`.)

- [ ] **Step 6: Update all `newAuthProvider` call sites** to pass `nil` for the file-reading path:
  - `proxy.go:55` (`buildProviders`): `newAuthProvider(prov.Provider, name, cfg, nil)` — **Task 4 changes this to pass the bound cred**.
  - `main.go` `fetchZhipuQuota`, `showGenericUsage`, `fetchDeepseekQuota`, and any other `newAuthProvider(...)` call: add `, nil` now; Task 4/8 will pass real creds.

- [ ] **Step 7: Thread `*accountCred` through the usage/quota functions.** Change signatures:

`main.go`:
```go
func fetchZhipuQuota(cfg *Config, name string, prov Provider, cred *accountCred) (*provider.QuotaSnapshot, error) {
	auth := newAuthProvider(prov.Provider, name, cfg, cred)
	// ...rest unchanged...
}
func showGenericUsage(cfg *Config, provName string, prov Provider, cred *accountCred) {
	auth := newAuthProvider(prov.Provider, provName, cfg, cred)
	// ...rest unchanged...
}
func fetchDeepseekQuota(cfg *Config, name string, prov Provider, cred *accountCred) (*provider.QuotaSnapshot, error) {
	auth := newAuthProvider(prov.Provider, name, cfg, cred)
	// ...rest unchanged...
}
```

`provider_wire.go`:
```go
func showZhipuUsageData(cfg *Config, providerName string, prov Provider, cred *accountCred) (any, error) {
	showGenericUsage(cfg, providerName, prov, cred)
	return nil, nil
}
func showDeepseekUsageData(cfg *Config, providerName string, prov Provider, cred *accountCred) (any, error) {
	showDeepseekUsage(cfg, providerName, prov, cred)
	return nil, nil
}
func showVolcengineUsageData(cfg *Config, providerName string, prov Provider, cred *accountCred) (any, error) {
	showVolcengineUsage(cfg, providerName, prov, cred)
	return nil, nil
}
```
(If `showDeepseekUsage`/`showVolcengineUsage` call `newAuthProvider` internally, add the `cred` param there too, same pattern.)

- [ ] **Step 8: Fix the `buildProviders` closures** in `proxy.go` so they compile with the new signatures — for now pass `nil` (Task 4 binds real creds):

```go
case "zhipu":
	pcfg.UsageFn = func() (any, error) { return showZhipuUsageData(cfg, name, prov, nil) }
	pcfg.QuotaFn = func() (any, error) { return fetchZhipuQuota(cfg, name, prov, nil) }
```
(Same for deepseek/volcengine.)

- [ ] **Step 9: Build + full test, expect PASS**

Run: `cd model-proxy && go build ./... && go test ./...`
Expected: build clean, all existing tests pass (single-account paths pass `nil`, unchanged behavior).

- [ ] **Step 10: Commit**

```bash
git add model-proxy/
git commit -m "feat(auth): bind ApiKeyBase to in-memory key; thread accountCred"
```

---

## Task 4: `buildProviders` unrolling + index maps

**Files:**
- Modify: `model-proxy/proxy.go` (`buildProviders`, `Proxy` struct, `NewProxy`, `reload`)
- Test: `model-proxy/proxy_routing_test.go` (extend) or new `buildproviders_pool_test.go`

**Interfaces:**
- Produces: `Proxy.poolIndex map[string][]string` (parent → sorted virtual ids), `Proxy.parentOf map[string]string` (virtual → parent), `Proxy.spreadParents map[string]bool`. `buildProviders` returns the map of virtual ids; when a provider's pool has ≥2 accounts, the parent name is NOT a key (only virtuals are).

- [ ] **Step 1: Write the failing test** (`buildproviders_pool_test.go`)

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// writePoolFile writes a plural credential pool for `name`.
func writePoolFile(t *testing.T, name, providerID string, keys ...string) {
	t.Helper()
	p := credentialPool{Version: 1}
	for _, k := range keys {
		c := accountCred{APIKey: k}
		p.Accounts = append(p.Accounts, poolAccount{
			ID: accountIDFor(providerID, c), Label: k, APIKey: k, AddedAt: "2026-07-08",
		})
	}
	if err := savePool(name, p); err != nil {
		t.Fatal(err)
	}
}

func TestBuildProvidersUnrollsPool(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
		},
	}
	p := NewProxy(cfg)

	// Parent is not a runnable provider; 3 virtuals are.
	if _, ok := p.providers["zhipu"]; ok {
		t.Fatal("parent zhipu should not be in providers map when pooled")
	}
	var seen []string
	for k := range p.providers {
		seen = append(seen, k)
	}
	if len(seen) != 3 {
		t.Fatalf("want 3 virtuals, got %v", seen)
	}
	// Each virtual injects its own exact key (white-box: p.providers[vid] is a
	// provider.Provider whose AuthHeaders injects the bound key).
	want := map[string]bool{"Bearer KEY-A": true, "Bearer KEY-B": true, "Bearer KEY-C": true}
	for _, vid := range p.poolIndex["zhipu"] {
		req := httptest.NewRequest(http.MethodGet, "https://z/m", nil)
		if err := p.providers[vid].AuthHeaders(req); err != nil {
			t.Fatal(err)
		}
		tok := req.Header.Get("Authorization")
		if !want[tok] {
			t.Fatalf("virtual %s injected %q, want one of Bearer KEY-A/B/C", vid, tok)
		}
		delete(want, tok)
	}
	if len(want) != 0 {
		t.Fatalf("missing tokens: %v", want)
	}
	// parentOf reverse map correct.
	for _, vid := range p.poolIndex["zhipu"] {
		if p.parentOf[vid] != "zhipu" {
			t.Fatalf("parentOf[%s]=%q want zhipu", vid, p.parentOf[vid])
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `poolIndex` undefined / unrolling not present.

- [ ] **Step 3: Add the single-account backward-compat test** (same file) — pool size 1 must keep the plain provider name and leave `poolIndex` empty (unchanged single-account behavior):

```go
func TestBuildProvidersSingleAccountKeepsPlainName(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "SOLO")
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}}}
	p := NewProxy(cfg)
	if _, ok := p.providers["zhipu"]; !ok {
		t.Fatal("single account must keep plain name zhipu")
	}
	if len(p.poolIndex) != 0 {
		t.Fatalf("poolIndex should be empty for single accounts, got %v", p.poolIndex)
	}
}
```

- [ ] **Step 4: Implement unrolling** in `proxy.go`. Add fields to `Proxy`:

```go
type Proxy struct {
	mu           sync.RWMutex
	healthMu     sync.Mutex
	cfg          *Config
	providers    map[string]provider.Provider
	client       *http.Client
	health       map[string]*providerHealth
	sticky       map[string]routeSticky
	quota        *quotaTracker
	poolIndex    map[string][]string // parent → sorted virtual ids (only multi-account parents)
	parentOf     map[string]string   // virtual id → parent
	spreadParents map[string]bool
	spreadCtr    map[string]uint64 // parent → round-robin counter (healthMu)
	expandedRoutes map[string][]RouteTarget // exposed → expanded targets
}
```

Rewrite `buildProviders` to unroll. It must also build `poolIndex`/`parentOf`/`spreadParents` — since those are on `Proxy`, move pool loading + index building into a method called by `NewProxy`/`reload` after `buildProviders`. Concretely, split:

```go
// buildProviders builds virtual provider instances. For a provider whose
// credential pool has ≥2 accounts it returns one virtual per account keyed
// "name#<id>"; pool size 1 (or no pool) returns the plain name (unchanged).
func buildProviders(cfg *Config) map[string]provider.Provider {
	m := map[string]provider.Provider{}
	for name, prov := range cfg.Providers {
		pool, _ := loadPool(name, prov.Provider)
		if len(pool.Accounts) <= 1 {
			// single-account / legacy / not-logged-in: original path, cred=nil
			m[name] = buildOne(cfg, name, prov, accountCred{})
			continue
		}
		for _, a := range pool.Accounts {
			vid := name + "#" + a.ID
			m[vid] = buildOne(cfg, name, prov, a.cred())
		}
	}
	return m
}

// buildOne constructs a single provider instance (real or virtual) bound to cred.
func buildOne(cfg *Config, name string, prov Provider, cred accountCred) provider.Provider {
	auth := newAuthProvider(prov.Provider, name, cfg, nil)
	if cred.APIKey != "" {
		auth = newAuthProvider(prov.Provider, name, cfg, &cred)
	}
	pcfg := &provider.Config{ProviderID: prov.Provider, OpenAIBaseURL: prov.OpenAIBaseURL, Headers: prov.Headers, UsageURL: prov.UsageURL, Auth: authAdapter{auth}}
	switch prov.Provider {
	case "zhipu":
		pcfg.UsageFn = func() (any, error) { return showZhipuUsageData(cfg, name, prov, credOrNil(cred)) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchZhipuQuota(cfg, name, prov, credOrNil(cred)) }
	case "deepseek":
		pcfg.UsageFn = func() (any, error) { return showDeepseekUsageData(cfg, name, prov, credOrNil(cred)) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchDeepseekQuota(cfg, name, prov, credOrNil(cred)) }
	case "volcengine":
		pcfg.UsageFn = func() (any, error) { return showVolcengineUsageData(cfg, name, prov, credOrNil(cred)) }
		pcfg.FetchModelsFn = func() ([]string, error) { return listArkAgentPlanModelIDs(name) }
		// Volcengine quota is V4-signed with AK/SK; cred binding is added in
		// Task 10. Until then, pass the existing file-reading fetch.
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchVolcengineQuota(name) }
	case "aqp":
		pcfg.LoginFn = func() error { return runLogin(cfg) }
		pcfg.LogoutFn = func() error { return clearAccount(authFilePath("aqp", "oauth_auth")) }
		pcfg.UsageFn = func() (any, error) { return showAqpUsageData(cfg) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchAqpQuota(cfg) }
	case "codex":
		pcfg.LoginFn = func() error { return runCodexLogin(cfg) }
		pcfg.LogoutFn = func() error { return clearCodexAuth(cfg) }
		pcfg.UsageFn = func() (any, error) { return showCodexUsageData(cfg, prov) }
		pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCodexQuota(cfg, prov) }
	}
	p, err := provider.New(pcfg, name)
	if err != nil {
		log.Printf("[proxy] failed to build provider %s: %v (using auth-only)", name, err)
		return nil
	}
	return p
}

func credOrNil(c accountCred) *accountCred {
	if c.APIKey == "" {
		return nil
	}
	return &c
}
```

Add the index builder (called in `NewProxy` + `reload`):

```go
func (p *Proxy) buildPoolIndex() {
	p.poolIndex = map[string][]string{}
	p.parentOf = map[string]string{}
	p.spreadParents = map[string]bool{}
	for name, prov := range p.cfg.Providers {
		pool, _ := loadPool(name, prov.Provider)
		if len(pool.Accounts) < 2 {
			continue
		}
		var vids []string
		for _, a := range pool.Accounts {
			vid := name + "#" + a.ID
			vids = append(vids, vid)
			p.parentOf[vid] = name
		}
		sort.Strings(vids)
		p.poolIndex[name] = vids
		if prov.Strategy == "spread" {
			p.spreadParents[name] = true
		}
	}
}
```

Wire into `NewProxy` (after `buildProviders`) and `reload` (after rebuilding providers):

```go
p.buildPoolIndex()
p.expandedRoutes = p.buildExpandedRoutes() // Task 5
p.spreadCtr = map[string]uint64{}
```

(`buildOne` returns `nil` on failure — `buildProviders` callers already skip nil.)

- [ ] **Step 5: Run, expect PASS**

Run: `cd model-proxy && go test -run 'TestBuildProviders' .`
Expected: PASS — 3 distinct virtuals, single-account keeps plain name.

- [ ] **Step 6: Commit**

```bash
git add model-proxy/
git commit -m "feat(proxy): unroll credential pool into virtual providers + index maps"
```

---

## Task 5: Route expansion

**Files:**
- Modify: `model-proxy/proxy.go` (`buildExpandedRoutes`, `forward`, `scheduleStatus`)
- Test: `model-proxy/proxy_routing_test.go` (extend)

**Interfaces:**
- Produces: `Proxy.expandedRoutes map[string][]RouteTarget`; `forward` + `scheduleStatus` read it instead of `cfg.Routes`. A pooled target fans out to its N virtual children (same `Model` + `Priority`).

- [ ] **Step 1: Write the failing test**

```go
func TestRouteExpansionFansOutPool(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes: map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}},
	}
	p := NewProxy(cfg)
	got := p.expandedRoutes["glm-5"]
	if len(got) != 2 {
		t.Fatalf("expanded len = %d, want 2", len(got))
	}
	for _, tg := range got {
		if tg.Model != "glm-5" {
			t.Fatalf("model = %q, want glm-5", tg.Model)
		}
		if p.parentOf[tg.Provider] != "zhipu" {
			t.Fatalf("expanded target provider %q not a zhipu virtual", tg.Provider)
		}
	}
	// non-pooled provider passes through unchanged.
	cfg2 := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes: map[string][]RouteTarget{"m": {{Provider: "z", Model: "m"}}},
	}
	p2 := NewProxy(cfg2) // single account (not logged in → 0 accounts → not pooled)
	if len(p2.expandedRoutes["m"]) != 1 || p2.expandedRoutes["m"][0].Provider != "z" {
		t.Fatalf("non-pooled target should pass through: %v", p2.expandedRoutes["m"])
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `expandedRoutes`/`buildExpandedRoutes` undefined.

- [ ] **Step 3: Implement** `buildExpandedRoutes`:

```go
// buildExpandedRoutes returns routes with pooled targets fanned out to their
// virtual children. A target whose provider is a pooled parent (poolIndex) is
// replaced by its N virtuals, each with the same Model + Priority.
func (p *Proxy) buildExpandedRoutes() map[string][]RouteTarget {
	out := map[string][]RouteTarget{}
	for exposed, targets := range p.cfg.Routes {
		var exp []RouteTarget
		for _, t := range targets {
			vids, pooled := p.poolIndex[t.Provider]
			if !pooled {
				exp = append(exp, t)
				continue
			}
			for _, vid := range vids {
				exp = append(exp, RouteTarget{Provider: vid, Model: t.Model, Priority: t.Priority})
			}
		}
		out[exposed] = exp
	}
	return out
}
```

Call it in `NewProxy` + `reload` (after `buildPoolIndex`).

- [ ] **Step 4: Switch call sites.** In `forward` (`proxy.go:403`):

```go
	targets, ok := p.expandedRoutes[exposed]
```

In `scheduleStatus` (`proxy.go:274`):

```go
	for exposed, targets := range p.expandedRoutes {
```

(`doctor`, Task 9, uses a standalone expand for the offline path.)

- [ ] **Step 5: Run, expect PASS**: `cd model-proxy && go test -run TestRouteExpansion .`

- [ ] **Step 6: Commit**

```bash
git add model-proxy/
git commit -m "feat(proxy): expand pooled route targets to virtual children"
```

---

## Task 6: `decideOrder` spread branch + `spreadCtr` + `commit`

**Files:**
- Modify: `model-proxy/proxy.go` (`decideOrder`, `schedule`, `scheduleStatus`)
- Test: `model-proxy/proxy_routing_test.go` (extend) — round-robin + circuit-skip + sticky regression.

**Interfaces:**
- Consumes: `Proxy.spreadParents`, `Proxy.parentOf`, `Proxy.spreadCtr`, `cfg.Providers[*].Strategy` (via Task 2).
- Produces: `decideOrder(..., commit bool)`; spread routes order their band by stable account-id rotated by a per-parent counter (bumped only when `commit`), and skip sticky parking.

- [ ] **Step 1: Write the failing test** (round-robin distribution)

```go
func TestSpreadRoundRobinEven(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu", Strategy: "spread"}},
		Routes: map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}},
	}
	p := NewProxy(cfg)
	// 6 scheduled orders → each account is first exactly twice.
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		ordered := p.schedule(cfg, p.providers, "glm-5", p.expandedRoutes["glm-5"])
		if len(ordered) == 0 {
			t.Fatal("empty order")
		}
		counts[p.parentOf[ordered[0].Provider]+"#"+ordered[0].Provider]++
	}
	if len(counts) != 3 {
		t.Fatalf("want 3 distinct first-picks, got %v", counts)
	}
	for k, n := range counts {
		if n != 2 {
			t.Fatalf("account %s served %d times as first, want 2", k, n)
		}
	}
}
```

Add the circuit-skip test:

```go
func TestSpreadSkipsCircuitOpenAccount(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu", Strategy: "spread"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := NewProxy(cfg)
	// Force one virtual's circuit open.
	var blocked string
	for _, vid := range p.poolIndex["zhipu"] {
		blocked = vid
		break
	}
	p.healthMu.Lock()
	p.health[blocked] = &providerHealth{circuitOpenUntil: time.Now().Add(time.Hour)}
	p.healthMu.Unlock()
	// 6 schedules: blocked account never first; other two split.
	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		ordered := p.schedule(cfg, p.providers, "glm-5", p.expandedRoutes["glm-5"])
		counts[ordered[0].Provider]++
	}
	if counts[blocked] != 0 {
		t.Fatalf("blocked account was first %d times", counts[blocked])
	}
	if len(counts) != 2 {
		t.Fatalf("want 2 active accounts, got %v", counts)
	}
}
```

Add a sticky regression test (default strategy unchanged):

```go
func TestStickyParksOnOneAccount(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}}, // no strategy → sticky
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := NewProxy(cfg)
	first := p.schedule(cfg, p.providers, "glm-5", p.expandedRoutes["glm-5"])[0].Provider
	for i := 0; i < 3; i++ {
		got := p.schedule(cfg, p.providers, "glm-5", p.expandedRoutes["glm-5"])[0].Provider
		if got != first {
			t.Fatalf("sticky drifted: first=%s got=%s", first, got)
		}
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — spread doesn't round-robin yet.

- [ ] **Step 3: Implement.** Change `schedule` to pass `commit=true`:

```go
func (p *Proxy) schedule(cfg *Config, provs map[string]provider.Provider, exposed string, targets []RouteTarget) []RouteTarget {
	now := time.Now()
	ordered, stickyToSet := p.decideOrder(cfg, provs, exposed, targets, now, true)
	if stickyToSet != "" {
		p.healthMu.Lock()
		p.sticky[exposed] = routeSticky{provider: stickyToSet, since: now}
		p.healthMu.Unlock()
	}
	return ordered
}
```

Change `scheduleStatus` to pass `commit=false` (peek): `p.decideOrder(cfg, provs, exposed, targets, now, false)`.

Rewrite `decideOrder` with the spread branch + `commit`:

```go
func (p *Proxy) decideOrder(cfg *Config, provs map[string]provider.Provider, exposed string, targets []RouteTarget, now time.Time, commit bool) (ordered []RouteTarget, stickyToSet string) {
	sched := cfg.Scheduling
	var qs map[string]*provider.QuotaSnapshot
	if p.quota != nil {
		qs = p.quota.allSnapshots()
	}

	p.healthMu.Lock()
	defer p.healthMu.Unlock()

	avail := func(name string) bool {
		h := p.health[name]
		return h == nil || h.available(now)
	}
	var availTargets []RouteTarget
	for _, t := range targets {
		if avail(t.Provider) {
			availTargets = append(availTargets, t)
		}
	}

	// ---- spread path ----
	if p.isSpreadRoute(targets) {
		return p.spreadOrder(cfg, availTargets, qs, now, commit), ""
	}

	// ---- existing sticky path (unchanged) ----
	billingOf := func(name string) provider.BillingClass { return p.billingClass(cfg, name, qs) }
	surplusOf := func(name string) float64 { /* unchanged */ }
	sort.SliceStable(availTargets, func(i, j int) bool { /* unchanged tier→priority→surplus */ })
	margin := sched.switchMargin()
	cur := p.sticky[exposed]
	// ... (existing cur/keepSticky logic, byte-for-byte) ...
	return ordered, stickyToSet
}

// isSpreadRoute reports whether any target's parent provider has strategy: spread.
func (p *Proxy) isSpreadRoute(targets []RouteTarget) bool {
	for _, t := range targets {
		if parent, ok := p.parentOf[t.Provider]; ok && p.spreadParents[parent] {
			return true
		}
	}
	return false
}

// spreadOrder orders available targets for a spread route: each spread parent's
// virtuals are ordered by stable virtual id and rotated by a per-parent counter
// (bumped when commit); non-spread targets keep tier→priority→surplus order and
// merge by tier. Round-robin is over available members only.
func (p *Proxy) spreadOrder(cfg *Config, avail []RouteTarget, qs map[string]*provider.QuotaSnapshot, now time.Time, commit bool) []RouteTarget {
	// Split into spread bands (per parent) and the rest.
	type band struct{ parent string; members []RouteTarget }
	bands := map[string][]RouteTarget{}
	var rest []RouteTarget
	for _, t := range avail {
		parent, ok := p.parentOf[t.Provider]
		if ok && p.spreadParents[parent] {
			bands[parent] = append(bands[parent], t)
		} else {
			rest = append(rest, t)
		}
	}
	// Rest ranks by tier→priority→surplus (existing comparator).
	sort.SliceStable(rest, func(i, j int) bool {
		ri, rj := tierRank(p.billingClass(cfg, rest[i].Provider, qs)), tierRank(p.billingClass(cfg, rest[j].Provider, qs))
		if ri != rj { return ri < rj }
		if rest[i].Priority != rest[j].Priority { return rest[i].Priority < rest[j].Priority }
		return surplusCompare(p, cfg, qs, rest[i].Provider, rest[j].Provider)
	})
	// Each spread band: stable id order, rotated by counter.
	var ordered []RouteTarget
	for parent, members := range bands {
		sort.SliceStable(members, func(i, j int) bool { return members[i].Provider < members[j].Provider })
		start := int(p.spreadCtr[parent]) % len(members)
		if commit {
			p.spreadCtr[parent]++
		}
		for i := 0; i < len(members); i++ {
			ordered = append(ordered, members[(start+i)%len(members)])
		}
	}
	// Merge: spread bands are plan tier (rank 0); interleave by tier with rest.
	all := append(ordered, rest...)
	sort.SliceStable(all, func(i, j int) bool {
		return tierRank(p.billingClass(cfg, all[i].Provider, qs)) < tierRank(p.billingClass(cfg, all[j].Provider, qs))
	})
	return all
}
```

Extract the surplus comparator used by both paths:

```go
func surplusCompare(p *Proxy, cfg *Config, qs map[string]*provider.QuotaSnapshot, a, b string) bool {
	sa := p.surplusOf(cfg, qs, a, time.Now())
	sb := p.surplusOf(cfg, qs, b, time.Now())
	return sa > sb
}
```

(Factor the existing `surplusOf` closure in `decideOrder` into a method `p.surplusOf(cfg, qs, name, now)` so both `decideOrder` and `spreadOrder` share it — DRY. Move its body verbatim.)

- [ ] **Step 4: Run, expect PASS**: `cd model-proxy && go test -run 'TestSpread|TestStickyParks' . -race`

- [ ] **Step 5: Commit**

```bash
git add model-proxy/
git commit -m "feat(sched): strategy:spread round-robin in decideOrder (peek/commit)"
```

---

## Task 7: `login` pool CLI — dedup, `--label`, `--replace`, reload signal

**Files:**
- Modify: `model-proxy/login.go` (`runApiKeyLogin`), `model-proxy/main.go` (`cmdLogin`)
- Test: `model-proxy/login_test.go` (extend)

**Interfaces:**
- Produces: pool-aware `runApiKeyLogin` writing the plural pool; `cmdLogin` parses `--label`/`--replace` and signals reload.

- [ ] **Step 1: Write the failing test**

```go
func TestRunApiKeyLoginDedupSameKey(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	prov := cfg.Providers["zhipu"]
	// First login (no pool yet) via direct pool write helper.
	writePoolFile(t, "zhipu", "zhipu", "DUP-KEY")
	// Re-enter the SAME key with --replace: pool size must stay 1.
	runApiKeyLoginWithInput(cfg, "zhipu", prov, "DUP-KEY", "", true /*replace*/)
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("dup key should keep size 1, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
}

func TestRunApiKeyLoginDifferentKeyAppends(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	writePoolFile(t, "zhipu", "zhipu", "KEY-1")
	runApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "KEY-2", "team", false)
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 2 {
		t.Fatalf("different key should append, got %d", len(pool.Accounts))
	}
	// label applied
	var labeled *poolAccount
	for i := range pool.Accounts {
		if pool.Accounts[i].Label == "team" {
			labeled = &pool.Accounts[i]
		}
	}
	if labeled == nil || labeled.APIKey != "KEY-2" {
		t.Fatalf("labeled entry wrong: %+v", labeled)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `runApiKeyLoginWithInput` undefined.

- [ ] **Step 3: Implement.** Refactor `runApiKeyLogin` to take the key + label + replace and write the pool:

```go
// runApiKeyLoginWithInput performs a pool-aware login: prompt is bypassed when
// in is non-empty (tests). It dedups by accountID; same id with replace=true
// overwrites, else appends. label is applied to the entry.
func runApiKeyLoginWithInput(cfg *Config, provName string, prov Provider, in, label string, replace bool) error {
	key := strings.TrimSpace(in)
	if key == "" {
		fmt.Printf("Enter API key for %s: ", provName)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read API key: %w", err)
		}
		key = strings.TrimSpace(line)
	}
	if key == "" {
		return fmt.Errorf("empty API key")
	}
	// Validate (existing logic): 401/403 reject.
	if prov.UsageURL != "" {
		// ...existing validation block, using `key`...
	}
	// Pool dedup.
	pool, _ := loadPool(provName, prov.Provider)
	id := accountIDFor(prov.Provider, accountCred{APIKey: key})
	now := nowTS()
	idx := -1
	for i, a := range pool.Accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx >= 0 {
		if !replace {
			fmt.Printf("Account %q is already logged in. Replace its key? [y/N] ", pool.Accounts[idx].Label)
			reader := bufio.NewReader(os.Stdin)
			ans, _ := reader.ReadString('\n')
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
				return fmt.Errorf("login cancelled")
			}
		}
		pool.Accounts[idx].APIKey = key
		if label != "" {
			pool.Accounts[idx].Label = label
		}
		pool.Accounts[idx].AddedAt = now
	} else {
		lbl := label
		if lbl == "" {
			lbl = id
		}
		pool.Accounts = append(pool.Accounts, poolAccount{ID: id, Label: lbl, APIKey: key, AddedAt: now})
	}
	if err := savePool(provName, pool); err != nil {
		return fmt.Errorf("save pool: %w", err)
	}
	fmt.Println(cGreen("✓ Saved account ") + cGray(mask(id)+" ("+labelFor(pool, id)+")"))
	return nil
}

func labelFor(pool credentialPool, id string) string {
	for _, a := range pool.Accounts {
		if a.ID == id {
			return a.Label
		}
	}
	return id
}
```

Keep the old `runApiKeyLogin(cfg, provName, prov)` as a thin wrapper calling `runApiKeyLoginWithInput(cfg, provName, prov, "", "", false)` so the existing provider-callback path still works.

`cmdLogin` (in `login.go`): parse `--label`/`--replace` (reuse the repo's flag helpers; if none, scan `args`), then after a successful login, signal the daemon to reload:

```go
func cmdLogin(args []string) {
	// ...existing config load + provName resolution...
	label := flagStringValue(args, "--label")
	replace := hasFlagValue(args, "--replace")
	prov := cfg.Providers[provName]
	if err := runApiKeyLoginWithInput(cfg, provName, prov, "", label, replace); err != nil {
		log.Fatalf("login failed: %v", err)
	}
	// Signal a running daemon to hot-reload so the new account is live.
	maybeReloadDaemon()
}
```

(`flagStringValue`/`hasFlagValue` — add to `util.go` if absent, or reuse existing arg helpers; check `util.go` first. `maybeReloadDaemon` sends SIGHUP to the pid from the log path if present, no-op otherwise — mirror `cmdReload`'s signal logic.)

> Note: only the apikey providers (zhipu/deepseek/volcengine) go through this path. `aqp`/`codex` keep their existing `runLogin`/`runCodexLogin` flows (phase 2). Guard `cmdLogin`: if `prov.Provider` is aqp/codex, call the existing path.

- [ ] **Step 4: Run, expect PASS**: `cd model-proxy && go test -run TestRunApiKeyLogin .`

- [ ] **Step 5: Commit**

```bash
git add model-proxy/
git commit -m "feat(login): pool-aware login with dedup, --label, --replace, reload"
```

---

## Task 8: `logout` + `usage` pool CLI

**Files:**
- Modify: `model-proxy/main.go` (`cmdLogout`, `cmdUsage`)
- Test: `model-proxy/cli_test.go` (extend)

**Interfaces:**
- Consumes: `Proxy.poolIndex` (via `buildProviders`), `loadPool`/`savePool`.

- [ ] **Step 1: Write the failing tests** (capture stdout via the repo's existing test capture helper; if absent, assert on pool-file state + a buffered stdout).

```go
func TestCmdLogoutInteractiveRemovesOne(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	// Choose the first account (index 1) on stdin.
	setStdin(t, "1\n")
	cmdLogout([]string{"zhipu"})
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("after logout want 1 account, got %d", len(pool.Accounts))
	}
}

func TestCmdUsagePoolPrintsAllAccounts(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	// Stub the usage fetch to a deterministic per-account marker.
	out := captureStdout(t, func() { cmdUsage([]string{"zhipu"}) })
	id1 := accountIDFor("zhipu", accountCred{APIKey: "K1"})
	id2 := accountIDFor("zhipu", accountCred{APIKey: "K2"})
	if !strings.Contains(out, id1[:8]) || !strings.Contains(out, id2[:8]) {
		t.Fatalf("usage output missing per-account headers; got:\n%s", out)
	}
}
```

(`setStdin`, `setHome`, `captureStdout` — use the repo's existing helpers in `util_test.go`/`cli_test.go`; add minimal versions if missing.)

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `cmdLogout`** pool-aware:

```go
func cmdLogout(args []string) {
	cfg, _ := LoadConfig(configPath(args))
	provName := positional(args)
	if provName == "" { /* existing usage listing */ return }
	label := flagStringValue(args, "--label")
	all := hasFlagValue(args, "--all")

	pool, _ := loadPool(provName, cfg.Providers[provName].Provider)
	if len(pool.Accounts) == 0 {
		fmt.Println(cYellow("Not logged in."))
		return
	}
	var rmID string
	switch {
	case all:
		pool.Accounts = nil
	case label != "":
		idx := -1
		for i, a := range pool.Accounts {
			if a.Label == label { idx = i; break }
		}
		if idx < 0 { log.Fatalf("no account labeled %q in %s", label, provName) }
		rmID = pool.Accounts[idx].ID
		pool.Accounts = append(pool.Accounts[:idx], pool.Accounts[idx+1:]...)
	default:
		// Interactive: list + pick.
		fmt.Printf("Accounts for %s:\n", provName)
		for i, a := range pool.Accounts {
			fmt.Printf("  %d) %s  (#%s  added %s)\n", i+1, a.Label, mask(a.ID), a.AddedAt)
		}
		fmt.Print("Remove which (number)? ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(pool.Accounts) {
			log.Fatal("invalid selection")
		}
		rmID = pool.Accounts[n-1].ID
		pool.Accounts = append(pool.Accounts[:n-1], pool.Accounts[n:]...)
	}
	if len(pool.Accounts) == 0 {
		os.Remove(poolPath(provName)) // pool empty → delete plural file
	} else {
		savePool(provName, pool)
	}
	fmt.Println(cGreen("✓ Removed account " + mask(rmID)))
	maybeReloadDaemon()
}
```

(Again, guard aqp/codex to their existing single-file logout path.)

- [ ] **Step 4: Implement `cmdUsage`** to iterate pool children. When `provName` is a pooled parent (`poolIndex[provName]` non-empty), loop its virtuals and call each `Usage()`:

```go
func cmdUsage(args []string) {
	// ...existing no-arg-all-providers path...
	pv := buildProviders(cfg) // virtuals
	p := NewProxy(cfg)        // for poolIndex (cheap; no daemon)
	vids := p.poolIndex[provName]
	if len(vids) > 0 {
		for _, vid := range vids {
			fmt.Println(cDim("────────────────────────────────────────"))
			fmt.Printf("%s  (%s)\n", cCyan(provName), mask(vid))
			if _, err := pv[vid].Usage(); err != nil {
				fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
			}
		}
		return
	}
	// single-account / non-pooled path (existing)
	if p.providers[provName] == nil { log.Fatalf("unknown provider %q", provName) }
	if _, err := pv[provName].Usage(); err != nil { log.Fatalf("usage failed: %v", err) }
}
```

- [ ] **Step 5: Run, expect PASS**: `cd model-proxy && go test -run 'TestCmdLogout|TestCmdUsagePool' .`

- [ ] **Step 6: Commit**

```bash
git add model-proxy/
git commit -m "feat(cli): pool-aware logout (interactive/label/all) + usage all accounts"
```

---

## Task 9: `models refresh` once + observability grouping

**Files:**
- Modify: `model-proxy/models.go` (`cmdModels` refresh path → runs once)
- Modify: `model-proxy/main.go` (`cmdSchedule`, `cmdDoctor`) — group virtuals under parent
- Test: `model-proxy/models_test.go`, `model-proxy/cli_extra_test.go` (extend)

- [ ] **Step 1: Write the failing test** — `refreshProviderModels` with a 3-account pool hits the upstream `/models` exactly once (drive it against an httptest server that counts hits):

```go
func TestRefreshProviderModelsOnceForPool(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2", "K3")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5","object":"model"}]}`))
	}))
	defer srv.Close()
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: srv.URL + "/v1", Provider: "zhipu"}}}
	entries, err := refreshProviderModels(cfg, "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("/models hit %d times, want 1 (pool must not fan out)", hits)
	}
	if len(entries) == 0 || entries[0] != "glm-5" {
		t.Fatalf("entries = %v, want [glm-5]", entries)
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.** Extract a testable `refreshProviderModels(cfg, provName) ([]string, error)` that resolves a pooled parent to its **first** virtual (or the plain name if single-account) and fetches once — never fanning out across the pool:

```go
// refreshProviderModels fetches the live model list for a provider exactly once.
// If `provName` is a pooled parent it uses the pool's first account; the model
// list is per-upstream, not per-account, so one fetch is correct and sufficient.
func refreshProviderModels(cfg *Config, provName string) ([]string, error) {
	pv := buildProviders(cfg)
	target := provName
	if vids, pooled := poolVirtuals(cfg, provName); pooled {
		target = vids[0] // first virtual by account-id order
	}
	impl, ok := pv[target]
	if !ok || impl == nil {
		return nil, fmt.Errorf("provider %q not available (not logged in?)", provName)
	}
	return impl.FetchModels()
}

// poolVirtuals returns the sorted virtual ids for a pooled parent (true), or
// (nil, false) for a single-account / not-logged-in provider. Reads the pool
// file directly so callers (models, doctor) don't need a running Proxy.
func poolVirtuals(cfg *Config, name string) ([]string, bool) {
	prov := cfg.Providers[name]
	pool, _ := loadPool(name, prov.Provider)
	if len(pool.Accounts) < 2 {
		return nil, false
	}
	var vids []string
	for _, a := range pool.Accounts {
		vids = append(vids, name+"#"+a.ID)
	}
	sort.Strings(vids)
	return vids, true
}
```

In `cmdModels`, replace the refresh fetch call with `refreshProviderModels(cfg, provName)`:

```go
		fmt.Fprintf(os.Stderr, "Refreshing models from %s...\n", provName)
		entries, err := refreshProviderModels(cfg, provName)
```

(`poolVirtuals` also replaces `poolIndexLookup` for `doctor` in Step 4 — one helper, no full Proxy needed.)

- [ ] **Step 4: Observability grouping.** In `scheduleStatus` output + `cmdSchedule`/`cmdDoctor` rendering: when a provider name is a pooled parent, render a header `zhipu (N accounts)` then its virtuals indented. Concretely, in the JSON builder (`scheduleStatus`), nest virtuals:

```go
	// group: if exposed route's targets share a pooled parent, emit parent + children
```

Add a `"pooled"` grouping to the `routeInfo` JSON (parent label + children list). For `cmdDoctor` (offline), group via `poolIndexLookup(cfg, name)`.

- [ ] **Step 5: Run, expect PASS**: `cd model-proxy && go test -run 'TestModelsRefresh|TestSchedule|TestDoctor' .`

- [ ] **Step 6: Commit**

```bash
git add model-proxy/
git commit -m "feat(cli): models refresh once per pool; group virtuals in schedule/doctor"
```

---

## Task 10: volcengine pool — AK/SK per account

**Files:**
- Modify: `model-proxy/login.go` (volcengine login writes `{api_key, access_key, secret_key}` per pool entry), `main.go` (`fetchVolcengineQuota` bound to cred's AK/SK)
- Test: `model-proxy/volcengine_creds_test.go` (extend)

- [ ] **Step 1: Write the failing test** — a 2-account volcengine pool yields 2 virtuals each with its own AK; `fetchVolcengineQuota` for a virtual uses that virtual's AK/SK (stub the V4-signed call; assert exact AK used).

```go
func TestVolcenginePoolPerAccountAK(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	p := credentialPool{Version: 1, Accounts: []poolAccount{
		{ID: "AK1", Label: "a", APIKey: "k1", AccessKey: "AK1", SecretKey: "SK1", AddedAt: "x"},
		{ID: "AK2", Label: "b", APIKey: "k2", AccessKey: "AK2", SecretKey: "SK2", AddedAt: "x"},
	}}
	savePool("volcengine", p)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"volcengine": {OpenAIBaseURL: "https://v", Provider: "volcengine"}}}
	px := NewProxy(cfg)
	if len(px.poolIndex["volcengine"]) != 2 { t.Fatal("want 2 virtuals") }
	// accountID = access_key
	want := map[string]bool{"volcengine#AK1": true, "volcengine#AK2": true}
	for _, vid := range px.poolIndex["volcengine"] {
		if !want[vid] { t.Fatalf("unexpected virtual %q", vid) }
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (volcengine login still writes singular file).

- [ ] **Step 3: Implement.** `runVolcengineLogin` writes the full triple into a pool entry (same dedup pattern as `runApiKeyLoginWithInput`, but the entry carries AK/SK and `accountID = access_key`). Add `fetchVolcengineQuotaBound(name, cred)` that uses `cred.AccessKey/SecretKey` when non-empty, else reads the file (existing `fetchVolcengineQuota`). Wire it in `buildOne` (Task 4 already references it).

- [ ] **Step 4: Run, expect PASS**: `cd model-proxy && go test -run TestVolcenginePool .`

- [ ] **Step 5: Commit**

```bash
git add model-proxy/
git commit -m "feat(volcengine): per-account AK/SK in credential pool"
```

---

## Final verification

- [ ] **Full suite + gates**

```bash
cd model-proxy
gofmt -l .          # empty
go vet ./...        # clean
go test ./...       # all pass
go test -race ./... # race-clean
scripts/cover.sh    # ≥80% per package
```

- [ ] **Manual smoke** (document in `docs/superpowers/specs/2026-07-07-multi-account-load-balancing-design.md` or AGENTS.md):
  - `model-proxy login zhipu` twice with two real keys → `usage zhipu` shows both.
  - Set `strategy: spread` → under concurrent requests both accounts serve.
  - `logout zhipu` interactive removes one.

- [ ] **Update CLAUDE.md / AGENTS.md** — add the pool concept, `strategy: spread`, plural pool file convention, and the `zhipu#acctN` virtual naming to the provider/credentials sections.

```bash
git add docs/ model-proxy/ CLAUDE.md AGENTS.md
git commit -m "docs: multi-account credential pool + strategy:spread"
```
