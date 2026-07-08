package main

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestWeb(t *testing.T) (*webServer, *Proxy) {
	t.Helper()
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p := NewProxy(cfg)
	return newWebServer(p, "test-config.yaml"), p
}

func TestAPIStatus(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	// Exact structural fields must be present (shape check on key presence).
	for _, want := range []string{`"uptime"`, `"version"`, `"listen"`, `"health"`, `"schedule"`, `"counters"`} {
		if !strings.Contains(body, want) {
			t.Errorf("status body missing %s: %s", want, body)
		}
	}
}

// TestAPIStatusUnknown404 asserts the catch-all still 404s for unknown /api paths
// once the first real route (/api/status) is wired. Guards against a future
// router change silently swallowing unknown paths.
func TestAPIStatusUnknown404(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/no-such-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown /api path status=%d want 404", rec.Code)
	}
}

func TestWebServesUI(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /ui/ status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("/ui/ body missing doctype: %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/ui/app.js", nil))
	if rec2.Code != 200 {
		t.Fatalf("GET /ui/app.js status=%d want 200", rec2.Code)
	}
}

func TestServeUITraversalGuard(t *testing.T) {
	w, _ := newTestWeb(t)

	// Substitute a fake FS that serves a known body for ANY path, including
	// traversal-shaped names. We do this because embed.FS itself rejects ".."
	// via fs.ValidPath — so without this seam the test cannot tell whether the
	// 404 came from the guard or from embed.FS. With the fake FS, a request
	// that gets past the guard would return 200 (fake body), making the guard
	// the sole thing under test: delete the guard and this test goes red.
	origFS := webFS
	webFS = fakeServeAllFS{body: []byte("SECRET")}
	t.Cleanup(func() { webFS = origFS })

	// Call serveUI directly. The real ServeMux canonicalizes "/ui/../x" → "/x"
	// before the handler runs, so going through ServeHTTP would never exercise
	// the in-handler guard. httptest.NewRequest does NOT canonicalize, so the
	// path arrives with name="../etc/passwd" and the ".." guard fires.
	rec := httptest.NewRecorder()
	w.serveUI(rec, httptest.NewRequest("GET", "/ui/../etc/passwd", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("traversal path returned %d, want 404 (guard must block before ReadFile)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Errorf("traversal path leaked fake FS body — guard did not fire before ReadFile")
	}

	// Sanity: a normal asset still serves (proves the fake FS is wired and the
	// code path reaches ReadFile for non-traversal names).
	rec2 := httptest.NewRecorder()
	w.serveUI(rec2, httptest.NewRequest("GET", "/ui/app.js", nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("app.js returned %d, want 200", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "SECRET") {
		t.Errorf("app.js did not serve fake FS body — seam not wired correctly")
	}
}

// fakeServeAllFS is an fs.FS that Open's any path and serves a fixed body. It
// is used only by TestServeUITraversalGuard to isolate the in-handler ".."
// guard from embed.FS's own (redundant) traversal protection.
type fakeServeAllFS struct {
	body []byte
}

func (f fakeServeAllFS) Open(name string) (fs.File, error) {
	return &fakeFile{name: name, r: bytes.NewReader(f.body)}, nil
}

type fakeFile struct {
	name string
	r    *bytes.Reader
}

func (f *fakeFile) Stat() (fs.FileInfo, error) {
	return fakeFileInfo{name: f.name, size: f.r.Size()}, nil
}

func (f *fakeFile) Read(p []byte) (int, error) {
	if f.r.Len() == 0 {
		return 0, io.EOF
	}
	return f.r.Read(p)
}

func (f *fakeFile) Close() error { return nil }

type fakeFileInfo struct {
	name string
	size int64
}

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return fi.size }
func (fi fakeFileInfo) Mode() fs.FileMode  { return 0444 }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return false }
func (fi fakeFileInfo) Sys() any           { return nil }

func TestConfigPutValid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	original := []byte("listen: 127.0.0.1:17000\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	os.WriteFile(cfgPath, original, 0o644)

	w, p := newTestWeb(t)
	w.configFile = cfgPath
	edited := []byte("listen: 127.0.0.1:17001\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	rec := httptest.NewRecorder()
	body := `{"yaml":"` + strings.ReplaceAll(strings.ReplaceAll(string(edited), "\n", "\\n"), `"`, `\"`) + `"}`
	w.handleConfigPut(rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(got, edited) {
		t.Errorf("config not written; got %q want %q", got, edited)
	}
	bak, _ := os.ReadFile(cfgPath + ".bak")
	if !bytes.Equal(bak, original) {
		t.Errorf("backup not original; got %q", bak)
	}
	// reload happened: p.cfg.Listen updated.
	if p.cfg.Listen != "127.0.0.1:17001" {
		t.Errorf("reload did not apply: listen=%q", p.cfg.Listen)
	}
}

func TestConfigPutInvalidNoWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	original := []byte("listen: 127.0.0.1:17000\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	os.WriteFile(cfgPath, original, 0o644)

	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	// invalid: route references a missing provider
	bad := []byte("listen: 127.0.0.1:17000\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\nroutes:\n  m: [{provider: ghost, model: m}]\n")
	rec := httptest.NewRecorder()
	body := `{"yaml":"` + strings.ReplaceAll(strings.ReplaceAll(string(bad), "\n", "\\n"), `"`, `\"`) + `"}`
	w.handleConfigPut(rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(got, original) {
		t.Errorf("invalid write should not touch config; got %q", got)
	}
	if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Errorf("invalid write should not create a backup")
	}
}

func TestAPILogs(t *testing.T) {
	tmp := t.TempDir() + "/model-proxy.log"
	logContent := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(tmp, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestWeb(t)
	w.logFile = tmp // override hook for tests

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	w.register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/logs?tail=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	got := rec.Body.String()
	// last 2 lines
	if !strings.Contains(got, "line4") || !strings.Contains(got, "line5") {
		t.Errorf("tail=2 missing last lines: %q", got)
	}
	if strings.Contains(got, "line1") {
		t.Errorf("tail=2 should drop line1: %q", got)
	}
}

func TestConfigEditPreservesComments(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	original := []byte("# top comment\nlisten: 127.0.0.1:17000 # inline\n# scheduling block\nscheduling:\n  circuit_threshold: 3\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	os.WriteFile(cfgPath, original, 0o644)
	w, p := newTestWeb(t)
	w.configFile = cfgPath

	rec := httptest.NewRecorder()
	body := `{"kind":"scheduling","data":{"circuit_threshold":5}}`
	w.handleConfigEdit(rec, httptest.NewRequest("POST", "/api/config/edit", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	gs := string(got)
	if !strings.Contains(gs, "# top comment") {
		t.Errorf("top comment lost:\n%s", gs)
	}
	if !strings.Contains(gs, "# scheduling block") {
		t.Errorf("scheduling comment lost:\n%s", gs)
	}
	if !strings.Contains(gs, "circuit_threshold: 5") {
		t.Errorf("threshold not updated to 5:\n%s", gs)
	}
	if p.cfg.Scheduling.CircuitThreshold != 5 {
		t.Errorf("reload did not apply threshold 5: %d", p.cfg.Scheduling.CircuitThreshold)
	}
}

func TestConfigEditGeneral(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:17000\nlog_level: info\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	w.handleConfigEdit(rec, httptest.NewRequest("POST", "/api/config/edit", strings.NewReader(`{"kind":"general","data":{"listen":"127.0.0.1:18000"}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "listen: 127.0.0.1:18000") {
		t.Errorf("listen not updated:\n%s", got)
	}
}

// TestConfigEditUnknownKind asserts a bogus kind returns 400, not 500 or a
// panic. provider/route/claude_mapping are valid kinds as of Task 9
// (covered by TestConfigEditProviderBilling / TestConfigEditRouteCRUD), so this
// test now only covers the genuinely unknown case.
func TestConfigEditUnknownKind(t *testing.T) {
	w, _ := newTestWeb(t)
	for _, kind := range []string{"bogus"} {
		rec := httptest.NewRecorder()
		body := `{"kind":"` + kind + `","name":"x","data":{}}`
		w.handleConfigEdit(rec, httptest.NewRequest("POST", "/api/config/edit", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("kind=%s status=%d want 400 body=%s", kind, rec.Code, rec.Body.String())
		}
	}
}

func TestConfigEditProviderBilling(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("providers:\n  deepseek:\n    provider_id: deepseek\n    openai_base_url: https://api.deepseek.com\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	w.handleConfigEdit(rec, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"provider","name":"deepseek","data":{"billing":"plan"}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "billing: plan") {
		t.Errorf("billing not set:\n%s", got)
	}
}

// TestAccountsListMasked asserts /api/accounts NEVER serializes any secret
// (api_key / access_key / secret_key / SSO cookie) while still emitting the
// real account id (the UI needs it to remove accounts). The acct struct has no
// field for any secret — by construction they can't be serialized — and the
// test verifies the raw api_key string is absent from the body AND the real id
// is present.
func TestAccountsListMasked(t *testing.T) {
	setPoolHome(t, t.TempDir())
	if err := savePool("zhipu", credentialPool{
		Version: 1,
		Accounts: []poolAccount{{
			ID:        "abc1234567890def",
			Label:     "work",
			APIKey:    "sk-secret-key-1234567890",
			AccessKey: "AK-LEAK-12345",
			SecretKey: "SK-LEAK-67890",
			AddedAt:   "2026-01-01T00:00:00Z",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestWeb(t)
	rec := httptest.NewRecorder()
	w.handleAccountsList(rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// No raw secret may appear anywhere in the response.
	for _, secret := range []string{"sk-secret-key-1234567890", "AK-LEAK-12345", "SK-LEAK-67890"} {
		if strings.Contains(body, secret) {
			t.Errorf("raw secret leaked (%s):\n%s", secret, body)
		}
	}
	// The real account id MUST be present (unmasked) — the UI sends it back on
	// remove, so masking it would break deletion.
	if !strings.Contains(body, `"id":"abc1234567890def"`) {
		t.Errorf("real id (for removal) missing:\n%s", body)
	}
	if !strings.Contains(body, `"label":"work"`) {
		t.Errorf("label missing:\n%s", body)
	}
}

func TestConfigEditRouteCRUD(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\nroutes:\n  m: [{provider: zhipu, model: m}]\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	w.handleConfigEdit(rec, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"route","name":"m","data":{"targets":[{"provider":"zhipu","model":"m","priority":1}]}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "priority: 1") {
		t.Errorf("route target not updated:\n%s", got)
	}
}
