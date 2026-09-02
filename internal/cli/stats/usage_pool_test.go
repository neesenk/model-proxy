package stats

import (
	"model-proxy/internal/accounts"
	"model-proxy/internal/cli/clitest"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Test: `usage zhipu` with a 2-account pool iterates each account, printing a
// per-account header (label + masked id) and dispatching one usage fetch per
// account with THAT account's API key (captured by the httptest.Server).
func TestCLI_UsagePoolPrintsAllAccounts(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
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

	clitest.WritePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := clitest.WriteZhipuPoolConfig(t, srv.URL)

	out := clitest.GrabStdout(t, func() { RunUsage([]string{"zhipu", "--config", cfgPath}) })

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
	id1 := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "K1"})
	id2 := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "K2"})
	if !strings.Contains(out, "K1") {
		t.Errorf("output missing K1 label:\n%s", out)
	}
	if !strings.Contains(out, "K2") {
		t.Errorf("output missing K2 label:\n%s", out)
	}
	if m := accounts.Mask(id1); !strings.Contains(out, m) {
		t.Errorf("output missing masked id for K1 (%q):\n%s", m, out)
	}
	if m := accounts.Mask(id2); !strings.Contains(out, m) {
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
	clitest.SetPoolHome(t, dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	clitest.WritePoolFile(t, "zhipu", "zhipu", "K1", "K2")
	cfgPath := clitest.WriteZhipuPoolConfig(t, srv.URL)

	out := clitest.GrabStdout(t, func() { RunUsage([]string{"--config", cfgPath}) })

	if !strings.Contains(out, "K1") || !strings.Contains(out, "K2") {
		t.Errorf("all-providers usage missing pool account labels:\n%s", out)
	}
	if c := strings.Count(out, "───"); c < 2 {
		t.Errorf("want >=2 divider blocks, got %d:\n%s", c, out)
	}
}
