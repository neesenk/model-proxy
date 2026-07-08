package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// TestAPITokens verifies /api/tokens returns the snapshot and /api/tokens/reset
// zeros it. The snapshot key shape is {provider, model} → {input, output, ...}.
func TestAPITokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	p.tokens.commit(tokenKey{Provider: "zhipu", Model: "glm-5"}, tokenUsage{Input: 30, Output: 12})

	mux := http.NewServeMux()
	w.register(mux)

	// GET /api/tokens returns the committed usage.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/tokens", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"input":30`) || !strings.Contains(rec.Body.String(), `"output":12`) {
		t.Errorf("tokens body missing committed usage: %s", rec.Body.String())
	}

	// POST /api/tokens/reset clears the counter.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/tokens/reset", nil))
	if rec2.Code != 200 {
		t.Fatalf("reset status=%d want 200", rec2.Code)
	}
	if len(p.tokens.snapshot()) != 0 {
		t.Errorf("after reset, snapshot non-empty: %+v", p.tokens.snapshot())
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

// TestAccountsAddRemove covers the POST (add) + DELETE (remove) account flows.
// apikey add must validate against usage_url, save to the pool, and trigger a
// best-effort reload (ignored on failure — the account was still persisted).
// aqp/codex add must 400 (they use the async login flow, not the apikey core)
// rather than silently no-op'ing or attempting the apikey path. remove empties
// the pool; an unknown provider add must 404 (route table intact).
func TestAccountsAddRemove(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer up.Close()
	w, p := newTestWeb(t)
	w.configFile = "test" // reload will fail (no such file); add must still succeed
	p.mu.Lock()
	p.cfg.Providers["zhipu"] = Provider{Provider: "zhipu", OpenAIBaseURL: "https://x", UsageURL: up.URL}
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()

	// aqp/codex add must 400 — they use the async login flow (POST /api/login/<n>/start),
	// not the apikey core. Asserting the message points there keeps a future refactor
	// from accidentally swallowing these into the apikey path.
	for _, name := range []string{"aqp", "codex"} {
		rec := httptest.NewRecorder()
		w.handleAccountAdd(rec, httptest.NewRequest("POST", "/api/accounts/"+name,
			strings.NewReader(`{"api_key":"x"}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s add status=%d want 400 (async flow): %s", name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "/api/login/"+name+"/start") {
			t.Errorf("%s add must point at async flow: %s", name, rec.Body.String())
		}
	}

	// unknown provider add → 404 (route table + 404 contract intact).
	rec404 := httptest.NewRecorder()
	w.handleAccountAdd(rec404, httptest.NewRequest("POST", "/api/accounts/nope",
		strings.NewReader(`{"api_key":"x"}`)))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("unknown provider add status=%d want 404", rec404.Code)
	}

	// apikey add: validate (200 from usage mock) → save to pool → reload (best-effort).
	rec := httptest.NewRecorder()
	w.handleAccountAdd(rec, httptest.NewRequest("POST", "/api/accounts/zhipu",
		strings.NewReader(`{"api_key":"sk-test-1234567890","label":"work"}`)))
	if rec.Code != 200 {
		t.Fatalf("add status=%d body=%s", rec.Code, rec.Body.String())
	}
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("account not added: %+v", pool.Accounts)
	}
	if pool.Accounts[0].Label != "work" {
		t.Errorf("label not saved: %q", pool.Accounts[0].Label)
	}
	id := pool.Accounts[0].ID

	// remove → pool emptied.
	rec2 := httptest.NewRecorder()
	w.handleAccountRemove(rec2, httptest.NewRequest("DELETE", "/api/accounts/zhipu/"+id, nil))
	if rec2.Code != 200 {
		t.Fatalf("remove status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	pool2, _ := loadPool("zhipu", "zhipu")
	if len(pool2.Accounts) != 0 {
		t.Fatalf("account not removed: %+v", pool2.Accounts)
	}

	// remove with a missing id segment → 400 (not a panic / 500).
	rec3 := httptest.NewRecorder()
	w.handleAccountRemove(rec3, httptest.NewRequest("DELETE", "/api/accounts/zhipu", nil))
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("malformed remove status=%d want 400: %s", rec3.Code, rec3.Body.String())
	}

	// remove on unknown provider → 404 (route table intact).
	rec404b := httptest.NewRecorder()
	w.handleAccountRemove(rec404b, httptest.NewRequest("DELETE", "/api/accounts/nope/x", nil))
	if rec404b.Code != http.StatusNotFound {
		t.Errorf("remove unknown provider status=%d want 404", rec404b.Code)
	}

	// add JSON decode failure → 400.
	recBad := httptest.NewRecorder()
	w.handleAccountAdd(recBad, httptest.NewRequest("POST", "/api/accounts/zhipu",
		strings.NewReader(`{not-json`)))
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("bad-json add status=%d want 400: %s", recBad.Code, recBad.Body.String())
	}

	// add validation failure (usage_url 401) → 400 from the add core. Asserting
	// the message carries "validation failed" proves we surface the core's error
	// verbatim rather than masking it as a generic 400.
	badUp := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(401)
		rw.Write([]byte(`{"error":"bad key"}`))
	}))
	defer badUp.Close()
	p.mu.Lock()
	p.cfg.Providers["zhipu"] = Provider{Provider: "zhipu", OpenAIBaseURL: "https://x", UsageURL: badUp.URL}
	p.mu.Unlock()
	recVal := httptest.NewRecorder()
	w.handleAccountAdd(recVal, httptest.NewRequest("POST", "/api/accounts/zhipu",
		strings.NewReader(`{"api_key":"sk-bad"}`)))
	if recVal.Code != http.StatusBadRequest {
		t.Errorf("validation-failed add status=%d want 400: %s", recVal.Code, recVal.Body.String())
	}
	if !strings.Contains(recVal.Body.String(), "validation failed") {
		t.Errorf("validation error not surfaced: %s", recVal.Body.String())
	}
}

// TestAccountsRouting verifies the POST/DELETE cases are wired into serveAPI and
// stay distinct from GET /api/accounts (exact path) — guards against a future
// router change collapsing them or breaking path precedence.
func TestAccountsRouting(t *testing.T) {
	setPoolHome(t, t.TempDir())
	w, p := newTestWeb(t)
	w.configFile = "test"
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	mux := http.NewServeMux()
	w.register(mux)

	// GET /api/accounts → 200 (list, pre-existing).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/accounts status=%d want 200", rec.Code)
	}

	// POST /api/accounts/aqp → 400 (async flow), proving the POST prefix route is wired.
	recPost := httptest.NewRecorder()
	mux.ServeHTTP(recPost, httptest.NewRequest("POST", "/api/accounts/aqp", strings.NewReader(`{}`)))
	if recPost.Code != http.StatusBadRequest {
		t.Errorf("POST /api/accounts/aqp status=%d want 400: %s", recPost.Code, recPost.Body.String())
	}

	// DELETE /api/accounts/aqp/<id> → 200 (clearAccount on a nonexistent file is a no-op),
	// proving the DELETE prefix route is wired and distinct from POST.
	recDel := httptest.NewRecorder()
	mux.ServeHTTP(recDel, httptest.NewRequest("DELETE", "/api/accounts/aqp/some-id", nil))
	if recDel.Code != 200 {
		t.Errorf("DELETE /api/accounts/aqp/<id> status=%d want 200: %s", recDel.Code, recDel.Body.String())
	}

	// Unknown /api path still 404s.
	rec404 := httptest.NewRecorder()
	mux.ServeHTTP(rec404, httptest.NewRequest("GET", "/api/no-such", nil))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("unknown /api path status=%d want 404", rec404.Code)
	}
}

// TestAqpLoginFlow exercises the full async aqp SSO login: start bootstraps a
// login URL (against an httptest mock of the compass backend), a goroutine polls
// auth/info → fetchAPIKey → saveAccount, and poll returns "done" with the email.
// The account file must be persisted with the identity from get_or_generate.
func TestAqpLoginFlow(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Mock aqp backend: bootstrap returns a login URL; auth/info returns an
	// active user (and sets SSO_C so the jar captures it — mirroring the real
	// backend's 200 Set-Cookie); get_or_generate returns the managed key +
	// identity.
	mux := http.NewServeMux()
	mux.HandleFunc("/compass-api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"result":"https://soup.shopee.io/login"}`)
	})
	mux.HandleFunc("/compass-api/v1/auth/info", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: ssoCookieName, Value: "test-sso-c", Path: "/"})
		fmt.Fprint(w, `{"retcode":0,"data":{"user":{"userid":1,"email":"u@x.com","is_active":true}}}`)
	})
	mux.HandleFunc("/api/v1/cqp/ccswitch/api_key/get_or_generate", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"retcode":0,"data":{"api_key":"managed-key","project_id":"proj","employee_email":"u@x.com"}}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	// Seam: point the AQP client at the mock base so BootstrapLoginURL /
	// PollSession / fetchAPIKey hit the httptest server instead of the real
	// compass backend.
	w.newAqpClientFn = func(store string) *AqpClient { return newAqpClientWithBase(store, up.URL) }

	rec := httptest.NewRecorder()
	w.handleLoginStart(rec, httptest.NewRequest("POST", "/api/login/aqp/start", nil))
	if rec.Code != 200 {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		LoginURL  string `json:"login_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("parse start response: %v: %s", err, rec.Body.String())
	}
	if start.SessionID == "" || start.LoginURL == "" {
		t.Fatalf("bad start response: %s", rec.Body.String())
	}
	if start.LoginURL != "https://soup.shopee.io/login" {
		t.Errorf("login_url=%q want https://soup.shopee.io/login", start.LoginURL)
	}

	// Poll until done (the goroutine resolves quickly against the mock).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec2 := httptest.NewRecorder()
		w.handleLoginPoll(rec2, httptest.NewRequest("GET", "/api/login/"+start.SessionID+"/poll", nil))
		var st struct {
			State  string `json:"state"`
			Result string `json:"result"`
		}
		json.Unmarshal(rec2.Body.Bytes(), &st)
		if st.State == "done" {
			if st.Result != "u@x.com" {
				t.Errorf("poll result=%q want u@x.com", st.Result)
			}
			a, _ := loadAccount(authFilePath("aqp", "oauth_auth"))
			if a == nil {
				t.Fatal("aqp account file not written")
			}
			if a.Email != "u@x.com" {
				t.Errorf("persisted email=%q want u@x.com", a.Email)
			}
			if a.ProjectID != "proj" {
				t.Errorf("persisted project_id=%q want proj", a.ProjectID)
			}
			if a.SSOSessionCookie == "" {
				t.Error("persisted sso_session_cookie is empty")
			}
			return
		}
		if st.State == "error" {
			t.Fatalf("poll errored: %s", rec2.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("aqp login never completed")
}

// TestAqpLoginFlow_Error asserts the goroutine sets state="error" when the
// bootstrap itself fails (the mock returns no login URL). Guards against a
// silent hang where startAqpLogin returns 502 but the session never resolves.
func TestAqpLoginFlow_Error(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No "result" field → bootstrapAt fails to extract a login URL.
		w.WriteHeader(401)
		fmt.Fprint(w, `{"oops":"no url here"}`)
	}))
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newAqpClientFn = func(store string) *AqpClient { return newAqpClientWithBase(store, up.URL) }

	rec := httptest.NewRecorder()
	w.handleLoginStart(rec, httptest.NewRequest("POST", "/api/login/aqp/start", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("start status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
}

// TestLoginRouting verifies POST /api/login/<n>/start and GET
// /api/login/<id>/poll are wired into serveAPI and stay distinct from each
// other and from the existing /api/accounts routes.
func TestLoginRouting(t *testing.T) {
	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	mux := http.NewServeMux()
	w.register(mux)

	// POST /api/login/codex/start → 502 (requestUserCode fails against a dead
	// server), proving the POST prefix route is wired AND dispatches to the
	// codex branch (aqp would likewise try a real bootstrap). Forcing a dead
	// server keeps it deterministic — no real network dependency.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	w.newCodexOptions = func() *codexLoginServerOptions {
		o := &codexLoginServerOptions{usercodeURL: dead.URL + "/usercode"}
		o.defaults()
		return o
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/login/codex/start", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("POST /api/login/codex/start status=%d want 502: %s", rec.Code, rec.Body.String())
	}

	// POST /api/login/unknown/start → 404 (unknown provider).
	recUnk := httptest.NewRecorder()
	mux.ServeHTTP(recUnk, httptest.NewRequest("POST", "/api/login/unknown/start", nil))
	if recUnk.Code != http.StatusNotFound {
		t.Errorf("POST unknown provider status=%d want 404", recUnk.Code)
	}

	// GET /api/login/nope/poll → 404 (unknown session), proving the GET prefix
	// route is wired and distinct from POST.
	recPoll := httptest.NewRecorder()
	mux.ServeHTTP(recPoll, httptest.NewRequest("GET", "/api/login/nope/poll", nil))
	if recPoll.Code != http.StatusNotFound {
		t.Errorf("GET unknown session status=%d want 404", recPoll.Code)
	}
}

// TestCodexLoginFlow exercises the full async codex OAuth device-flow login:
// start requests a user code (against an httptest mock of the OpenAI deviceauth
// endpoints), a goroutine polls deviceauth/token → exchanges the code → writes
// the codex auth file 0600 → hot-reloads, and poll returns "done" with the
// account id parsed from the fake id_token JWT. Mirrors TestAqpLoginFlow's
// shape; the newCodexOptions seam points requestUserCode / pollForToken /
// exchangeCodeForTokens at the httptest mock's 3 endpoints.
func TestCodexLoginFlow(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Build a fake id_token JWT with a chatgpt_account_id claim (signature is
	// dummy — exchangeCodeForTokens only parses the payload claim, it doesn't
	// verify the sig). Replicates codex_login_test.go:127-128 inline.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}`))
	fakeIDToken := "h." + payload + ".s"
	// Mock the 3 codex OAuth endpoints (usercode → devtok → tok).
	mux := http.NewServeMux()
	mux.HandleFunc("/usercode", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"device_auth_id":"daid","user_code":"CODE","interval":"1"}`)
	})
	mux.HandleFunc("/devtok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_code":"ac","code_challenge":"cc","code_verifier":"cv"}`)
	})
	mux.HandleFunc("/tok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","id_token":"`+fakeIDToken+`"}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	// Seam: point codex options at the mock so requestUserCode / pollForToken /
	// exchangeCodeForTokens hit the httptest server instead of the real OpenAI
	// deviceauth endpoints.
	w.newCodexOptions = func() *codexLoginServerOptions {
		o := &codexLoginServerOptions{}
		o.defaults()
		o.usercodeURL = up.URL + "/usercode"
		o.deviceTokURL = up.URL + "/devtok"
		o.tokenURL = up.URL + "/tok"
		return o
	}

	rec := httptest.NewRecorder()
	w.handleLoginStart(rec, httptest.NewRequest("POST", "/api/login/codex/start", nil))
	if rec.Code != 200 {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		UserCode  string `json:"user_code"`
		VerifyURL string `json:"verify_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("parse start response: %v: %s", err, rec.Body.String())
	}
	if start.UserCode != "CODE" {
		t.Fatalf("user_code=%q want CODE: %s", start.UserCode, rec.Body.String())
	}
	if start.VerifyURL != codexOAuthVerifyURL {
		t.Errorf("verify_url=%q want %q", start.VerifyURL, codexOAuthVerifyURL)
	}
	if start.SessionID == "" {
		t.Fatal("session_id empty")
	}

	// Poll until done (the goroutine resolves quickly against the mock).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec2 := httptest.NewRecorder()
		w.handleLoginPoll(rec2, httptest.NewRequest("GET", "/api/login/"+start.SessionID+"/poll", nil))
		var st struct {
			State  string `json:"state"`
			Result string `json:"result"`
		}
		json.Unmarshal(rec2.Body.Bytes(), &st)
		if st.State == "done" {
			if st.Result != "acct-1" {
				t.Errorf("poll result=%q want acct-1", st.Result)
			}
			// codex auth file must exist (written 0600 by the goroutine).
			path := authFilePath("codex", "oauth_auth")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("codex auth file not written: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("codex auth file perm=%o want 0600", perm)
			}
			return
		}
		if st.State == "error" {
			t.Fatalf("poll errored: %s", rec2.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("codex login never completed")
}
