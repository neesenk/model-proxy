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
