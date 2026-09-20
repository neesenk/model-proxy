package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// TestMain redirects HOME to a throwaway dir for the whole package so no test
// ever reads or writes the real ~/.model-proxy state (quota_state.json, pool
// files, models cache, stats.db). Tests needing their own isolated dir still
// call t.Setenv("HOME", t.TempDir()). This is the package-level enforcement of
// the repo rule (AGENTS.md: tests must not touch the real HOME; the
// rule was on the books but unenforced — most NewProxy tests wrote the real
// state file).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "model-proxy-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	// os.Exit skips defers, so remove the temp HOME explicitly before exiting.
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// newTestProxy is the default constructor for functional tests. It guarantees
// every quota poller is stopped before its test TempDir/HOME is removed. The
// production NewProxy wrapper has one explicit path-wiring contract test below.
func newTestProxy(t testing.TB, cfg *configdomain.Config) *Proxy {
	t.Helper()
	return newTestProxyAt(t, cfg, filepath.Join(t.TempDir(), "quota_state.json"))
}

func newTestProxyAt(t testing.TB, cfg *configdomain.Config, statePath string) *Proxy {
	t.Helper()
	p := NewProxyWithStatePath(cfg, statePath)
	// Tests stay offline: Reload's best-effort models.dev refresh would hit
	// the real network from the shared TestMain HOME (above) and pollute it
	// for later exact-body /api/models assertions. Tests that need catalog
	// metadata replace this stub (TestReloadRefreshesCatalogThroughLoader) or
	// set p.catalog directly.
	p.catalogLoader = func(string, bool) (*catalog.Catalog, error) { return nil, nil }
	t.Cleanup(p.Close)
	return p
}

// TestNewProxy_DefaultStatePath is the deliberate exception to the test-helper
// rule: it covers the production wrapper's HOME resolution and default state
// path wiring. All other tests inject an isolated path through newTestProxy.
func TestNewProxy_DefaultStatePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{}}
	p := NewProxy(cfg)
	t.Cleanup(p.Close)
	want := filepath.Join(home, ".model-proxy", "quota_state.json")
	if p.quota.Path != want {
		t.Fatalf("NewProxy quota path = %q, want %q", p.quota.Path, want)
	}
}

// TestReloadRefreshesCatalogThroughLoader pins the reload catalog seam: the
// refresh runs through p.catalogLoader and installs the returned catalog for
// request-aware routing. This is the behavioral guard behind newTestProxyAt's
// offline stub — a wiring regression here would silently send every Reload
// test to the real models.dev endpoint and pollute the shared test HOME,
// instead of failing a test.
func TestReloadRefreshesCatalogThroughLoader(t *testing.T) {
	cfg, err := configdomain.LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	if p.catalogSnapshot() != nil {
		t.Fatal("fresh test proxy must start with no catalog")
	}
	calls := 0
	p.catalogLoader = func(homeDir string, _ bool) (*catalog.Catalog, error) {
		calls++
		if homeDir == "" {
			t.Error("loader homeDir must be non-empty")
		}
		return catalog.New(map[string]catalog.Model{"glm-5": {Context: 128}}), nil
	}
	yamlPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: https://x}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(yamlPath); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// The refresh is async (lifecycle goroutine): poll for the install.
	deadline := time.Now().Add(2 * time.Second)
	for p.catalogSnapshot() == nil {
		if time.Now().After(deadline) {
			t.Fatalf("reload never installed the loader's catalog (calls=%d)", calls)
		}
		time.Sleep(time.Millisecond)
	}
	if calls == 0 {
		t.Error("reload must route the catalog refresh through catalogLoader")
	}
	if m, ok := p.catalogSnapshot().Lookup("glm-5"); !ok || m.Context != 128 {
		t.Fatalf("installed catalog metadata = %+v, want glm-5 ctx 128", m)
	}
}

// TestQuotaPersist_ConcurrentTrackersNoRenameRace (bug 3): two+ trackers
// sharing one state file — as parallel NewProxy tests under one binary create —
// must not lose a rename to ENOENT. The fixed ".tmp" name guarded only by a
// per-instance mutex made the loser's rename fail (the visible symptom: the
// /api/health/reset handler returned 500 "persist failed: rename ...: no such
// file or directory"); unique same-dir temp files remove the contention and keep
// the final file valid (atomic rename → last writer wins).
func TestQuotaPersist_ConcurrentTrackersNoRenameRace(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/quota_state.json"
	cfg := func() *configdomain.Config { return &configdomain.Config{} }
	provs := func() map[string]provider.Provider { return nil }
	var wg sync.WaitGroup
	const trackers, persists = 3, 30
	for i := 0; i < trackers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := newStandaloneQuotaTracker(path, cfg, provs)
			for j := 0; j < persists; j++ {
				if err := tr.Persist(); err != nil {
					t.Errorf("persist failed: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file missing after persists: %v", err)
	}
	if !json.Valid(data) {
		t.Errorf("state file not valid JSON after concurrent persists: %s", data)
	}
}

// TestProxy_CloseStopsTracker (bug 3): Proxy.Close stops the quota tracker and
// drains its goroutine, so a test proxy doesn't leak a poller that fires a
// persist after the test (and its config generation) is gone. Close is
// idempotent — a second call must not deadlock.
func TestProxy_CloseStopsTracker(t *testing.T) {
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p := newTestProxy(t, cfg)
	select {
	case <-p.quota.StopChannel():
		t.Fatal("stopCh should be open before Close")
	default:
	}
	p.Close()
	select {
	case <-p.quota.StopChannel():
	default:
		t.Fatal("stopCh should be closed after Close")
	}
	p.Close() // idempotent: must not deadlock
}
