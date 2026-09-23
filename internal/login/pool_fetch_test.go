package login

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

func TestApiKeyLikeClassification(t *testing.T) {
	for _, id := range []string{"zhipu", "deepseek", "kimi-code", "mimo", "qwen-plan", "step-plan", "zcode", "static"} {
		if !ApiKeyLike(id) {
			t.Errorf("ApiKeyLike(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"aqp", "codex", "volcengine"} {
		if ApiKeyLike(id) {
			t.Errorf("ApiKeyLike(%q) = true, want false (OAuth/SSO/V4-signed)", id)
		}
	}
}

func TestLatestAPIKeyPicksNewestAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pool := `{"version":1,"accounts":[
	  {"id":"old","label":"old","api_key":"sk-old","added_at":"2026-01-01T00:00:00Z"},
	  {"id":"new","label":"new","api_key":"sk-new","added_at":"2026-06-01T00:00:00Z"}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "zhipu_apikeys.json"), []byte(pool), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LatestAPIKey("zhipu", "zhipu"); got != "sk-new" {
		t.Fatalf("LatestAPIKey = %q, want sk-new (newest added_at)", got)
	}
	if got := LatestAPIKey("absent", "zhipu"); got != "" {
		t.Fatalf("LatestAPIKey on missing pool = %q, want empty", got)
	}
}

func TestFetchVisibleModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-x" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m2"},{"id":"m1"}]}`))
	}))
	t.Cleanup(srv.Close)

	prov := configdomain.Provider{OpenAIBaseURL: srv.URL}
	ids, err := FetchVisibleModels(prov, "sk-x")
	if err != nil || len(ids) != 2 || ids[0] != "m2" {
		t.Fatalf("FetchVisibleModels = (%v, %v)", ids, err)
	}
	// Bad key → HTTP error (non-200) → error, no panic.
	if _, err := FetchVisibleModels(prov, "sk-bad"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("401 must surface as error, got %v", err)
	}
	// Missing inputs → error without network.
	if _, err := FetchVisibleModels(configdomain.Provider{}, ""); err == nil {
		t.Fatal("empty base/key must error")
	}
}
