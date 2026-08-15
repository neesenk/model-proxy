package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"model-proxy/internal/accounts"
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
)

// -- C1: `models` lists all exposed models from config ---

func TestCLI_ModelsListsAll(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "models", cfg)
	if code != 0 {
		t.Fatalf("models exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") {
		t.Errorf("models output missing glm-5.2:\n%s", stdout)
	}
	if !strings.Contains(stdout, "aqp") {
		t.Errorf("models output missing provider name aqp:\n%s", stdout)
	}
}

// -- C2: `models <provider>` lists one provider's models ---

func TestCLI_ModelsOneProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "models", cfg, "aqp")
	if code != 0 {
		t.Fatalf("models aqp exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") {
		t.Errorf("models aqp output missing glm-5.2:\n%s", stdout)
	}
}

// -- C3: `models <unknown>` exits non-zero with an error ---

func TestCLI_ModelsUnknownProviderExits(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "models", cfg, "nope")
	if code == 0 {
		t.Error("models nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("models nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// -- C4: `doctor` on a valid config prints "config valid" ---

func TestCLI_DoctorValidConfig(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "doctor", cfg)
	if code != 0 {
		t.Fatalf("doctor exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "config valid") {
		t.Errorf("doctor output missing 'config valid':\n%s", stdout)
	}
}

// -- C5: `doctor` on an invalid config exits non-zero ---

func TestCLI_DoctorInvalidConfig(t *testing.T) {
	bad := `listen: 127.0.0.1:15721
providers:
  aqp:
    openai_base_url: https://x
    provider_id: aqp
    anthropic_base_url: https://x/v1   # invalid: ends with /v1
    models: []
routes: {}
`
	cfg := writeTempConfig(t, bad)
	stdout, _, code := runCLI(t, "doctor", cfg)
	if code == 0 {
		t.Error("doctor invalid config: exit=0 want non-zero")
	}
	if !strings.Contains(stdout, "config invalid") {
		t.Errorf("doctor invalid stdout missing 'config invalid':\n%s", stdout)
	}
}

// -- C6: `schedule` with no daemon running exits non-zero with a reach error ---

func TestCLI_ScheduleNoDaemon(t *testing.T) {
	// Use a port nothing is listening on to guarantee "cannot reach daemon".
	cfg := writeTempConfig(t, "listen: 127.0.0.1:1\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - m\nroutes:\n  m:\n    - {provider: aqp, model: m}\n")
	_, stderr, code := runCLI(t, "schedule", cfg)
	if code == 0 {
		t.Error("schedule no daemon: exit=0 want non-zero")
	}
	// Assert the specific "cannot reach" message (not a 3-way OR that a panic
	// stack trace would pass).
	if !strings.Contains(stderr, "cannot reach") {
		t.Errorf("schedule no-daemon stderr missing 'cannot reach':\n%s", stderr)
	}
}

// -- C7: `config check` on a valid config exits 0 ---

func TestCLI_ConfigCheck(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "config", cfg, "check")
	if code != 0 {
		t.Fatalf("config check exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "aqp") {
		t.Errorf("config check output missing provider aqp:\n%s", stdout)
	}
}

// -- C8: `config` with no subcommand exits non-zero ---

func TestCLI_ConfigNoSubcommand(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, _, code := runCLI(t, "config", cfg)
	if code == 0 {
		t.Error("config (no subcommand): exit=0 want non-zero")
	}
}

// -- Pool-aware logout / usage (Task 8) -------------------------------------
//
// Pool CLI tests cover the three logout modes (interactive / --label / --all)
// and the per-account usage iteration. Happy paths run in-process (no log.Fatal
// on the success branch); fatal branches (label not found, invalid selection)
// use the subprocess pattern via runCLI / runCLIWithStdin. Tests that touch
// os.Stdin / os.Stdout run sequentially (none call t.Parallel).

// Test: `logout zhipu` interactive removes exactly the selected account.
// savePool sorts accounts by ID, so we compute which account is at index 0
// before the test, select it, and assert it's the one removed.
func TestCLI_LogoutInteractiveRemovesAccount(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := writeZhipuPoolConfig(t, "https://zhipu.invalid/u")

	pool, _ := app.LoadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 2 {
		t.Fatalf("setup: want 2 accounts, got %d", len(pool.Accounts))
	}
	firstID := pool.Accounts[0].ID
	secondID := pool.Accounts[1].ID

	setStdin(t, "1\n") // pick the first account (1-based)
	RunLogout([]string{"zhipu", "--config", cfgPath})

	pool2, _ := app.LoadPool("zhipu", "zhipu")
	if len(pool2.Accounts) != 1 {
		t.Fatalf("after logout want 1 account, got %d: %+v", len(pool2.Accounts), pool2.Accounts)
	}
	if pool2.Accounts[0].ID != secondID {
		t.Errorf("remaining account id = %q, want %q (the unselected one)", pool2.Accounts[0].ID, secondID)
	}
	if pool2.Accounts[0].ID == firstID {
		t.Errorf("selected account %q was not removed", firstID)
	}
}

// Test: `logout zhipu --all` clears the entire pool and removes the plural
// pool file. The singular file is untouched (it never existed in pool mode).
func TestCLI_LogoutAllClearsPool(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	// A stale legacy singular file from before pooling: it must NOT come back
	// to life after `logout --all` removes the pool accounts.
	if err := os.WriteFile(app.AccountStore().LegacyPath("zhipu"), []byte(`{"api_key":"ancient"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeZhipuPoolConfig(t, "https://zhipu.invalid/u")

	RunLogout([]string{"zhipu", "--all", "--config", cfgPath})

	// The empty plural file stays as the authoritative credential tombstone
	// (provider-pools.md) — removing it would re-open the legacy fallback and
	// resurrect the old key.
	pool, err := app.AccountStore().Load("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("tombstone pool unreadable: %v", err)
	}
	if len(pool.Accounts) != 0 {
		t.Errorf("pool accounts = %d, want 0", len(pool.Accounts))
	}
	snapshot, err := app.AccountStore().LoadSnapshot("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != accounts.SourcePlural {
		t.Errorf("source = %v, want SourcePlural (tombstone must override legacy)", snapshot.Source)
	}
}

// Test: `logout zhipu --label K1` removes only K1, leaves K2.
func TestCLI_LogoutByLabel(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := writeZhipuPoolConfig(t, "https://zhipu.invalid/u")

	RunLogout([]string{"zhipu", "--label", "K1", "--config", cfgPath})

	pool, _ := app.LoadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account after --label logout, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	if pool.Accounts[0].APIKey != "K2" {
		t.Errorf("K1 should be removed; remaining key = %q want K2", pool.Accounts[0].APIKey)
	}
}

// Test: `logout zhipu --label nope` exits non-zero with "no account labeled"
// (subprocess — RunLogout calls log.Fatalf on missing label).
func TestCLI_LogoutLabelNotFoundExits(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	writePoolFile(t, "zhipu", "zhipu", "K1")
	cfgPath := writeZhipuPoolConfig(t, "https://zhipu.invalid/u")

	_, stderr, code := runCLIWithHome(t, home, "logout", cfgPath, "zhipu", "--label", "nope")
	if code == 0 {
		t.Error("logout --label nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "no account labeled") {
		t.Errorf("stderr missing 'no account labeled':\n%s", stderr)
	}
}

// Test: `logout zhipu` interactive with an out-of-range number exits non-zero
// with "invalid selection" (subprocess with stdin piped).
func TestCLI_LogoutInvalidSelectionExits(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := writeZhipuPoolConfig(t, "https://zhipu.invalid/u")

	_, stderr, code := runCLIWithStdin(t, "99\n", home, "logout", cfgPath, "zhipu")
	if code == 0 {
		t.Error("logout invalid selection: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "invalid selection") {
		t.Errorf("stderr missing 'invalid selection':\n%s", stderr)
	}
}

// Test: `usage zhipu` with a 2-account pool iterates each account, printing a
// per-account header (label + masked id) and dispatching one usage fetch per
// account with THAT account's API key (captured by the httptest.Server).
func TestCLI_UsagePoolPrintsAllAccounts(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	var mu sync.Mutex
	var seenKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenKeys = append(seenKeys, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{}`)) // parseZhipuQuota returns nil → no quota body
	}))
	defer srv.Close()

	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := writeZhipuPoolConfig(t, srv.URL)

	out := grabStdout(t, func() { RunUsage([]string{"zhipu", "--config", cfgPath}) })

	// Each account's key was sent (proves per-account cred dispatch, not a
	// single shared key).
	want := map[string]bool{"Bearer K1": true, "Bearer K2": true}
	seen := map[string]bool{}
	for _, k := range seenKeys {
		if want[k] {
			seen[k] = true
		}
	}
	if len(seen) != 2 {
		t.Errorf("per-account keys not both sent; got %v want %v", seenKeys, want)
	}

	// Per-account headers: each label + masked id appears.
	id1 := app.AccountIDFor("zhipu", app.AccountCred{APIKey: "K1"})
	id2 := app.AccountIDFor("zhipu", app.AccountCred{APIKey: "K2"})
	if !strings.Contains(out, "K1") {
		t.Errorf("output missing K1 label:\n%s", out)
	}
	if !strings.Contains(out, "K2") {
		t.Errorf("output missing K2 label:\n%s", out)
	}
	if m := cliframework.Mask(id1); !strings.Contains(out, m) {
		t.Errorf("output missing masked id for K1 (%q):\n%s", m, out)
	}
	if m := cliframework.Mask(id2); !strings.Contains(out, m) {
		t.Errorf("output missing masked id for K2 (%q):\n%s", m, out)
	}
	// Two account blocks → at least two divider lines.
	if c := strings.Count(out, "───"); c < 2 {
		t.Errorf("want >=2 divider blocks, got %d:\n%s", c, out)
	}
}

// Test: `usage` (no provider) with a 2-account zhipu pool shows each account
// under the parent provider — the all-providers path must not skip pooled
// parents (regression: buildProviders[ parent ] is nil for pooled providers).
func TestCLI_UsageAllProvidersWithPool(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	writePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := writeZhipuPoolConfig(t, srv.URL)

	out := grabStdout(t, func() { RunUsage([]string{"--config", cfgPath}) })

	if !strings.Contains(out, "K1") || !strings.Contains(out, "K2") {
		t.Errorf("all-providers usage missing pool account labels:\n%s", out)
	}
	if c := strings.Count(out, "───"); c < 2 {
		t.Errorf("want >=2 divider blocks, got %d:\n%s", c, out)
	}
}
