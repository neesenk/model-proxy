package providerbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// authenticated_test.go covers AuthenticatedProviders — the offline,
// booleans-only projection of "which config-level providers can authenticate"
// that takeover prunes its model list by. It must agree with the runtime's
// authNotReady/expandTarget semantics: same BuildProviders pass (unbuilt ⇒
// not authenticated), same AuthReady reads, virtuals folded to parents,
// credential-free providers (static) authenticated whenever built, and the
// set NEVER carries false members (an all-logged-out config yields an EMPTY
// set — takeover's fresh-setup abstain keys off that).

func writeAuthFile(t *testing.T, home, base, content string) {
	t.Helper()
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func authConfig() *configdomain.Config {
	return &configdomain.Config{Providers: map[string]configdomain.Provider{
		"zhipu":       {OpenAIBaseURL: "http://x", Provider: "zhipu"},
		"opencode-go": {OpenAIBaseURL: "http://y", Provider: "opencode-go"},
		"codex":       {OpenAIBaseURL: "http://z", Provider: "codex"},
		"st":          {OpenAIBaseURL: "http://s", Provider: "static"},
	}}
}

// TestAuthenticatedProviders_MirrorsAuthReady pins the per-provider verdicts:
// legacy singular file ⇒ authenticated; missing credentials ⇒ ABSENT from the
// set (never a false member); codex authenticated iff its OAuth store holds a
// token; static authenticated whenever built (pool present), absent otherwise.
func TestAuthenticatedProviders_MirrorsAuthReady(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := authConfig()

	// Nothing logged in anywhere: the set is EMPTY (the fresh-setup abstain
	// contract — a false-member bug here would silently flip takeover into
	// pruning every model).
	if empty := AuthenticatedProvidersForHome(cfg, home); len(empty) != 0 {
		t.Fatalf("no logins: authed = %v, want empty set", empty)
	}

	// zhipu logs in (legacy singular file); codex gets an OAuth token store.
	writeAuthFile(t, home, "zhipu_apikey.json", `{"api_key":"SOLO"}`)
	writeAuthFile(t, home, "codex_oauth_auth.json", `{"tokens":{"access_token":"T","refresh_token":"R"}}`)
	got := AuthenticatedProvidersForHome(cfg, home)
	if !got["zhipu"] {
		t.Errorf("zhipu with legacy key must be authenticated: %v", got)
	}
	if !got["codex"] {
		t.Errorf("codex with OAuth tokens must be authenticated: %v", got)
	}
	if _, present := got["opencode-go"]; present {
		t.Errorf("opencode-go without login must be absent, set must carry only TRUE members: %v", got)
	}
	if got["st"] {
		t.Errorf("static without a pool must be absent from the set: %v", got)
	}
}

// TestAuthenticatedProviders_FoldsPoolVirtualsToParent: a multi-account pool
// authenticates the CONFIG-LEVEL parent name (routes target the parent; the
// runtime fans out per account) and no virtual "name#id" ever leaks into the
// set. static WITH a pool is built credential-free ⇒ authenticated.
func TestAuthenticatedProviders_FoldsPoolVirtualsToParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := authConfig()
	writePool(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	writePool(t, "st", "static", "STATIC-KEY")

	got := AuthenticatedProvidersForHome(cfg, home)
	if !got["zhipu"] {
		t.Fatalf("pooled zhipu must authenticate its parent name: %v", got)
	}
	if !got["st"] {
		t.Errorf("static with a pool must be authenticated: %v", got)
	}
	for name := range got {
		if strings.Contains(name, "#") {
			t.Errorf("set leaked virtual id %q (want parent names only): %v", name, got)
		}
	}
}
