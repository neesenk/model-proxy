package framework

import (
	"model-proxy/internal/accounts"
	"model-proxy/internal/display"
	"path/filepath"
	"strings"
	"testing"
)

// --- util.go ---

func TestAuthFilePath(t *testing.T) {
	got := AuthFilePath("zhipu-work", "apikey")
	if !strings.Contains(got, "zhipu-work_apikey.json") || !strings.Contains(got, ".model-proxy") {
		t.Errorf("authFilePath=%q want zhipu-work_apikey.json under .model-proxy", got)
	}
}

// --- color.go: root CLI color wrapper via the color-disabled path ---

func TestColorHelpers_NoColorPassthrough(t *testing.T) {
	// In tests stdout is not a tty (and NO_COLOR may be set), so color helpers
	// return the input verbatim. Assert the exact string rather than Contains,
	// which would also pass with ANSI codes around the input.
	for _, s := range []string{"x", "hello", "test-123"} {
		if got := display.Yellow(s); got != s {
			t.Errorf("display.Yellow(%q)=%q, want exact %q (no ANSI in test env)", s, got, s)
		}
	}
}

// --- env.go: LazyAccountStore home-keyed caching ---

// TestLazyAccountStoreCachesByHome pins the cache contract: same HOME reuses
// the store, a HOME change rebuilds it. Reuse-vs-rebuild is made observable
// via the process backend seam: a cached store must NOT pick up a backend
// flipped after the first Get, a rebuilt one must.
func TestLazyAccountStoreCachesByHome(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	t.Setenv("HOME", homeA)

	var s LazyAccountStore
	a1 := s.Get()
	if want := filepath.Join(homeA, ".model-proxy") + string(filepath.Separator); !strings.HasPrefix(a1.PoolPath("p"), want) {
		t.Errorf("PoolPath = %q, want under %q", a1.PoolPath("p"), want)
	}

	// Same HOME: reuse the cached store even though the process backend
	// changed since the first Get (a rebuild would observe the new backend,
	// making the Store value differ).
	accounts.SetProcessBackend(accounts.BackendKeychain)
	t.Cleanup(func() { accounts.SetProcessBackend(accounts.BackendFile) })
	if got := s.Get(); got != a1 {
		t.Error("same HOME rebuilt the store; want the cached one reused")
	}

	// HOME change: rebuild — the new store must be rooted at the new home AND
	// reflect the current process backend (identical to a fresh NewStore).
	t.Setenv("HOME", homeB)
	b1 := s.Get()
	if b1 == a1 {
		t.Error("HOME change returned the stale cached store")
	}
	if want := accounts.NewStore(homeB); b1 != want {
		t.Error("rebuilt store does not match a fresh NewStore for the new HOME")
	}
	if got := s.Get(); got != b1 {
		t.Error("second Get under the new HOME did not reuse the rebuilt store")
	}
}
