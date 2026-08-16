package provider

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApiKeyBaseWithKeyInjectsBoundKey(t *testing.T) {
	b := NewApiKeyBaseWithKey("zhipu", "BOUND-TOKEN")
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

// TestApiKeyBaseBoundRefreshIsNoOp asserts that Refresh on a bound (in-memory)
// ApiKeyBase is a no-op: the bound key is immutable (there's no file to re-read),
// so Refresh must NOT clear the cache. Without this guard, a 401-refresh-retry
// would leave LoadKey returning "" and AuthHeaders injecting "Bearer " upstream.
func TestApiKeyBaseBoundRefreshIsNoOp(t *testing.T) {
	b := NewApiKeyBaseWithKey("zhipu", "BOUND")
	if err := b.Refresh(); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	got, err := b.LoadKey()
	if err != nil || got != "BOUND" {
		t.Fatalf("LoadKey after Refresh = %q,%v want BOUND,nil", got, err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://x/v1/m", nil)
	if err := b.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders returned error: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer BOUND" {
		t.Fatalf("Authorization after Refresh = %q, want Bearer BOUND", got)
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

// TestApiKeyBaseWithKeySaveDeleteNoop asserts the bound instance never touches
// the filesystem (a pool-bound instance is never the source of truth for the
// auth file).
func TestApiKeyBaseWithKeySaveDeleteNoop(t *testing.T) {
	b := NewApiKeyBaseWithKey("zhipu", "BOUND")
	// Point authFile at a path that does not exist and would fail to mkdir;
	// SaveKey/DeleteKey must short-circuit and return nil without touching it.
	b.authFile = "/nonexistent-dir/should-not-be-written/apikey.json"
	if err := b.SaveKey("NEW"); err != nil {
		t.Fatalf("SaveKey on bound base returned error: %v", err)
	}
	if err := b.DeleteKey(); err != nil {
		t.Fatalf("DeleteKey on bound base returned error: %v", err)
	}
	// Bound key is still the cached one — SaveKey must not overwrite it.
	got, err := b.LoadKey()
	if err != nil || got != "BOUND" {
		t.Fatalf("LoadKey after SaveKey = %q,%v want BOUND,nil", got, err)
	}
}

// TestApiKeyBaseSaveKeyUnboundPersistsAtomically: the unbound (file-backed)
// SaveKey writes the key durably — file mode 0600, JSON round-trips, and no
// half-written temp file remains (temp+fsync+rename, pitfalls #18 pattern).
func TestApiKeyBaseSaveKeyUnboundPersistsAtomically(t *testing.T) {
	dir := t.TempDir()
	authFile := filepath.Join(dir, "sub", "auth.json")
	b := &ApiKeyBase{authFile: authFile}
	if err := b.SaveKey("sk-test-key-value"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	got, err := b.LoadKey()
	if err != nil || got != "sk-test-key-value" {
		t.Fatalf("LoadKey after SaveKey = %q,%v", got, err)
	}
	info, err := os.Stat(authFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode = %o, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(authFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("stray temp file left behind: %s", entry.Name())
		}
	}
}
