package catalog

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchHTTPConditionalAndErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != `"old"` {
			t.Errorf("If-None-Match=%q", got)
		}
		w.Header().Set("ETag", `"new"`)
		if r.URL.Path == "/not-modified" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, "body")
	}))
	defer server.Close()

	status, body, etag, err := FetchHTTP(server.URL, `"old"`)
	if err != nil || status != http.StatusOK || string(body) != "body" || etag != `"new"` {
		t.Fatalf("200=(%d,%q,%q,%v)", status, body, etag, err)
	}
	status, body, etag, err = FetchHTTP(server.URL+"/not-modified", `"old"`)
	if err != nil || status != http.StatusNotModified || body != nil || etag != `"new"` {
		t.Fatalf("304=(%d,%q,%q,%v)", status, body, etag, err)
	}
	if _, _, _, err := FetchHTTP("://bad", ""); err == nil {
		t.Fatal("malformed endpoint accepted")
	}
}

func TestCacheRoundTripAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	cat := New(map[string]Model{"m": {Context: 42, Modalities: Modalities{Input: []string{"text"}}}})
	cat.fetchedAt = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cat.etag = `"v1"`
	if err := save(path, cat); err != nil {
		t.Fatal(err)
	}
	loaded, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ETag() != `"v1"` || loaded.fetchedAt != cat.fetchedAt {
		t.Fatalf("round trip metadata=%q %s", loaded.ETag(), loaded.fetchedAt)
	}
	model, _ := loaded.Lookup("m")
	if model.Context != 42 || model.Modalities.Input[0] != "text" {
		t.Fatalf("round trip model=%+v", model)
	}
	if missing, err := load(filepath.Join(t.TempDir(), "missing")); err != nil || missing != nil {
		t.Fatalf("missing cache=(%+v,%v), want nil,nil", missing, err)
	}
	for _, invalid := range [][]byte{
		[]byte("not json"),
		[]byte(`{"fetched_at":"2026-07-01T00:00:00Z","by_name":{}}`),
	} {
		if err := os.WriteFile(path, invalid, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := load(path); err == nil {
			t.Fatalf("invalid cache accepted: %s", invalid)
		}
	}
}

func TestEnsureFreshBranches(t *testing.T) {
	makeOptions := func(path string, fetch FetchFunc) RefreshOptions {
		return RefreshOptions{CacheFile: path, Endpoint: "http://source", Fetch: fetch, Warnings: new(bytes.Buffer)}
	}
	t.Run("fresh cache skips fetch", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.json")
		seed := New(map[string]Model{"old": {Context: 1}})
		seed.fetchedAt = time.Now()
		if err := save(path, seed); err != nil {
			t.Fatal(err)
		}
		called := false
		cat, err := EnsureFresh(makeOptions(path, func(string, string) (int, []byte, string, error) { called = true; return 0, nil, "", nil }))
		if err != nil || called || cat.Count() != 1 {
			t.Fatalf("err=%v called=%v count=%d", err, called, cat.Count())
		}
	})
	t.Run("force sends etag and persists 200", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.json")
		seed := New(map[string]Model{"old": {Context: 1}})
		seed.fetchedAt = time.Now()
		seed.etag = `"old"`
		if err := save(path, seed); err != nil {
			t.Fatal(err)
		}
		var received string
		o := makeOptions(path, func(_ string, etag string) (int, []byte, string, error) {
			received = etag
			return 200, []byte(fixture), `"new"`, nil
		})
		o.Force = true
		cat, err := EnsureFresh(o)
		if err != nil || received != `"old"` || cat.ETag() != `"new"` || cat.Count() != 3 {
			t.Fatalf("err=%v etag=%q got=%q count=%d", err, received, cat.ETag(), cat.Count())
		}
		persisted, err := load(path)
		if err != nil || persisted.ETag() != `"new"` {
			t.Fatalf("persisted=%+v err=%v", persisted, err)
		}
	})
	t.Run("304 refreshes etag and persists", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.json")
		seed := New(map[string]Model{"old": {Context: 1}})
		seed.fetchedAt = time.Now().Add(-2 * DefaultTTL)
		seed.etag = `"old"`
		if err := save(path, seed); err != nil {
			t.Fatal(err)
		}
		var received string
		cat, err := EnsureFresh(makeOptions(path, func(_ string, etag string) (int, []byte, string, error) {
			received = etag
			return 304, nil, `"renewed"`, nil
		}))
		if err != nil || received != `"old"` || cat.ETag() != `"renewed"` || time.Since(cat.fetchedAt) > time.Minute {
			t.Fatalf("err=%v in=%q cat=%+v", err, received, cat)
		}
		persisted, err := load(path)
		if err != nil || persisted.ETag() != `"renewed"` {
			t.Fatalf("persisted=%+v err=%v", persisted, err)
		}
	})
	t.Run("stale survives malformed and 5xx with warnings", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			body   []byte
			err    error
		}{
			{"malformed syntax", 200, []byte("{"), nil},
			{"empty payload", 200, []byte("null"), nil},
			{"server error", 503, nil, nil},
			{"fetch error", 0, nil, os.ErrNotExist},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "cache.json")
				seed := New(map[string]Model{"old": {Context: 1}})
				seed.fetchedAt = time.Now().Add(-2 * DefaultTTL)
				if err := save(path, seed); err != nil {
					t.Fatal(err)
				}
				warnings := new(bytes.Buffer)
				o := makeOptions(path, func(string, string) (int, []byte, string, error) {
					return tc.status, tc.body, "", tc.err
				})
				o.Warnings = warnings
				cat, err := EnsureFresh(o)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := cat.Lookup("old"); !ok || !strings.Contains(warnings.String(), "using catalog cached") {
					t.Fatalf("cat=%+v warnings=%q", cat, warnings.String())
				}
				persisted, err := load(path)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := persisted.Lookup("old"); !ok {
					t.Fatalf("bad refresh overwrote stale disk cache: %+v", persisted)
				}
			})
		}
	})
	t.Run("corrupt cache is discarded and replaced by a valid response", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cache.json")
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		warnings := new(bytes.Buffer)
		o := makeOptions(path, func(string, string) (int, []byte, string, error) { return 200, []byte(fixture), `"fresh"`, nil })
		o.Warnings = warnings
		cat, err := EnsureFresh(o)
		if err != nil || cat.Count() != 3 || !strings.Contains(warnings.String(), "cache unreadable") {
			t.Fatalf("cat=%+v err=%v warnings=%q", cat, err, warnings.String())
		}
		persisted, err := load(path)
		if err != nil || persisted.ETag() != `"fresh"` {
			t.Fatalf("persisted=%+v err=%v", persisted, err)
		}
	})
	t.Run("no cache failures return empty plus error", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			body   []byte
			err    error
		}{
			{"malformed syntax", 200, []byte("{"), nil},
			{"empty payload", 200, []byte(`{"provider":{"models":{}}}`), nil},
			{"server error", 500, nil, nil},
			{"304", 304, nil, nil},
			{"fetch error", 0, nil, os.ErrNotExist},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cat, err := EnsureFresh(makeOptions(filepath.Join(t.TempDir(), "missing"), func(string, string) (int, []byte, string, error) {
					return tc.status, tc.body, "", tc.err
				}))
				if err == nil || cat == nil || cat.Count() != 0 {
					t.Fatalf("cat=%+v err=%v", cat, err)
				}
			})
		}
	})
}

func TestAtomicWriteUsesUniqueTempAndPreservesTargetOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models_cache.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("target=%q err=%v", got, err)
	}
	left, err := filepath.Glob(filepath.Join(dir, ".models_cache-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("temporary files=%v err=%v", left, err)
	}

	if err := atomicWriteWith(path, []byte("broken"), atomicWriteOps{
		createTemp: os.CreateTemp,
		rename:     func(_, _ string) error { return os.ErrPermission },
	}); !os.IsPermission(err) {
		t.Fatalf("rename error=%v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("failed rename exposed partial data=%q err=%v", got, err)
	}
	left, err = filepath.Glob(filepath.Join(dir, ".models_cache-*"))
	if err != nil || len(left) != 0 {
		t.Fatalf("failed rename leaked temp files=%v err=%v", left, err)
	}

	if err := atomicWriteWith(path, []byte("never"), atomicWriteOps{
		createTemp: func(string, string) (*os.File, error) { return nil, errors.New("create denied") },
		rename:     os.Rename,
	}); err == nil || !strings.Contains(err.Error(), "create denied") {
		t.Fatalf("create temp error=%v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("create failure changed target=%q err=%v", got, err)
	}
}

func TestAtomicWriteKeepsOldTargetVisibleUntilRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models_cache.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	renameStarted := make(chan string, 1)
	releaseRename := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- atomicWriteWith(path, []byte("new"), atomicWriteOps{
			createTemp: os.CreateTemp,
			rename: func(oldPath, newPath string) error {
				renameStarted <- oldPath
				<-releaseRename
				return os.Rename(oldPath, newPath)
			},
		})
	}()

	tmpPath := <-renameStarted
	released := false
	defer func() {
		if !released {
			close(releaseRename)
			<-done
		}
	}()
	beforeRename, err := os.ReadFile(path)
	if err != nil || string(beforeRename) != "old" {
		t.Fatalf("target before rename=%q err=%v, want complete old value", beforeRename, err)
	}
	staged, err := os.ReadFile(tmpPath)
	if err != nil || string(staged) != "new" {
		t.Fatalf("temporary value=%q err=%v, want complete new value", staged, err)
	}
	if filepath.Dir(tmpPath) != dir || tmpPath == path+".tmp" {
		t.Fatalf("temporary path=%q, want a unique sibling of target", tmpPath)
	}

	close(releaseRename)
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	afterRename, err := os.ReadFile(path)
	if err != nil || string(afterRename) != "new" {
		t.Fatalf("target after rename=%q err=%v, want complete new value", afterRename, err)
	}
}

func TestAge(t *testing.T) {
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Now(), "just now"},
		{time.Now().Add(-5 * time.Minute), "5m"},
		{time.Now().Add(-2 * time.Hour), "2h"},
	} {
		if got := age(tc.at); got != tc.want {
			t.Errorf("age(%v)=%q want %q", tc.at, got, tc.want)
		}
	}
}
