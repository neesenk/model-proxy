package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDiskCacheMemo pins the (mtime, size) keying: a content rewrite that
// preserves the file identity keeps serving the memoized catalog (the Status
// page's 5s poll must not re-decode the whole cache per tick), while a real
// refresh — an atomic write always lands a new file image — is picked up on
// the next Load. Missing and corrupt caches degrade to nil.
func TestDiskCacheMemo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models_cache.json")
	writeCache := func(t *testing.T, body string) time.Time {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}

	cache := NewDiskCache()
	if got := cache.Load(path); got != nil {
		t.Fatalf("Load(missing) = %v, want nil", got)
	}

	// Same byte length, different content: etag e1→e9 (count stays 1).
	first := `{"fetched_at":"2026-09-19T10:00:00Z","etag":"e1","by_name":{"m":{"ctx":1024,"out":256,"in":["text"],"out_mod":["text"]}}}`
	second := `{"fetched_at":"2026-09-19T10:00:00Z","etag":"e9","by_name":{"m":{"ctx":1024,"out":256,"in":["text"],"out_mod":["text"]}}}`
	if len(first) != len(second) {
		t.Fatalf("fixtures must be the same length: %d vs %d", len(first), len(second))
	}
	mtime := writeCache(t, first)
	got := cache.Load(path)
	if got == nil || got.Count() != 1 || got.ETag() != "e1" {
		t.Fatalf("first Load = %v, want count 1 etag e1", got)
	}
	// Rewrite the content behind the memo's identity: memoized value served.
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if got := cache.Load(path); got == nil || got.ETag() != "e1" {
		t.Fatalf("same-identity rewrite surfaced = %v, want memoized etag e1", got)
	}
	// A real refresh lands a new file image: the next Load sees it.
	writeCache(t, second)
	if got := cache.Load(path); got == nil || got.ETag() != "e9" {
		t.Fatalf("new file image = %v, want re-decoded etag e9", got)
	}

	// A corrupt cache degrades to nil instead of failing the read.
	writeCache(t, `{corrupt`)
	if got := cache.Load(path); got != nil {
		t.Fatalf("corrupt cache = %v, want nil", got)
	}
}
