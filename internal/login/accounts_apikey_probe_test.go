package login

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
)

// TestAddApikeyAccount_AuthlessModelsProbesRealRequest pins the login key
// validation for providers whose only candidate endpoint can never reject a key
// (ModelsAuthless && no usage_url — opencode-go: /models is public and ignores
// the Bearer). Validation must instead send ONE minimal real model request (the
// provider's ProbeRequest shape): a 401/403 (or an auth-failure envelope, or a
// network error) rejects the key; anything else accepts it. A garbage key must
// NOT enter the pool, and a valid key must.
func TestAddApikeyAccount_AuthlessModelsProbesRealRequest(t *testing.T) {
	var modelsHits, chatHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			// Public, auth-ignoring — exactly like the real opencode-go /models.
			modelsHits.Add(1)
			w.WriteHeader(200)
			w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.3"}]}`))
		case "/chat/completions":
			chatHits.Add(1)
			if r.Header.Get("Authorization") != "Bearer good-key" {
				w.WriteHeader(401)
				w.Write([]byte(`{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}`))
				return
			}
			w.WriteHeader(200)
			w.Write([]byte(`{"id":"x","choices":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()

	prov := configdomain.Provider{
		Provider:      "opencode-go",
		OpenAIBaseURL: upstream.URL,
		Models:        []string{"glm-5.3"},
	}
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"opencode-go": prov}}

	// Garbage key: rejected by the real-request probe, never pooled.
	setPoolHome(t, t.TempDir())
	if _, err := AddApikeyAccount(cfg, "opencode-go", prov, accountCred{APIKey: "garbage"}, "", true); err == nil ||
		!strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("garbage key: want 'validation failed', got %v", err)
	}
	pool, err := LoadPool("opencode-go", "opencode-go")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 0 {
		t.Fatalf("garbage key leaked into the pool: %+v", pool.Accounts)
	}
	if chatHits.Load() == 0 {
		t.Fatal("validation never sent the real-request probe")
	}

	// Valid key: probe 200 → pooled.
	setPoolHome(t, t.TempDir())
	id, err := AddApikeyAccount(cfg, "opencode-go", prov, accountCred{APIKey: "good-key"}, "", true)
	if err != nil {
		t.Fatalf("valid key: %v", err)
	}
	if id != accounts.AccountID("opencode-go", accountCred{APIKey: "good-key"}) {
		t.Fatalf("id = %q, want the key-derived account id", id)
	}
	pool, err = LoadPool("opencode-go", "opencode-go")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("valid key not pooled: %+v", pool.Accounts)
	}
}

// TestAddApikeyAccount_AuthlessModelsNoModelsConfigured pins fail-closed
// behavior when the provider config carries no model to probe with: login
// refuses with a clear error instead of falling back to the authless /models
// rubber-stamp.
func TestAddApikeyAccount_AuthlessModelsNoModelsConfigured(t *testing.T) {
	setPoolHome(t, t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200) // authless /models would accept anything
	}))
	defer upstream.Close()
	prov := configdomain.Provider{
		Provider:      "opencode-go",
		OpenAIBaseURL: upstream.URL,
	}
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"opencode-go": prov}}

	if _, err := AddApikeyAccount(cfg, "opencode-go", prov, accountCred{APIKey: "k"}, "", true); err == nil ||
		!strings.Contains(err.Error(), "no models") {
		t.Fatalf("no models configured: want a fail-closed 'no models' error, got %v", err)
	}
	pool, _ := LoadPool("opencode-go", "opencode-go")
	if len(pool.Accounts) != 0 {
		t.Fatalf("fail-closed must not pool: %+v", pool.Accounts)
	}
}

// TestAddApikeyAccount_AuthfulModelsUnchanged is the guardrail for providers
// whose /models DOES authenticate (qwen-plan shape: no usage_url, not
// ModelsAuthless): validation stays a Bearer GET /models — 401 rejects, 200
// accepts, and no chat probe is ever sent.
func TestAddApikeyAccount_AuthfulModelsUnchanged(t *testing.T) {
	var chatHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			chatHits.Add(1)
		}
		if r.Header.Get("Authorization") == "Bearer good-key" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(401)
	}))
	defer upstream.Close()
	prov := configdomain.Provider{
		Provider:      "qwen-plan",
		OpenAIBaseURL: upstream.URL,
		Models:        []string{"qwen3.8"},
	}
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"qwen-plan": prov}}

	setPoolHome(t, t.TempDir())
	if _, err := AddApikeyAccount(cfg, "qwen-plan", prov, accountCred{APIKey: "garbage"}, "", true); err == nil ||
		!strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("authful /models 401: want 'validation failed', got %v", err)
	}
	setPoolHome(t, t.TempDir())
	if _, err := AddApikeyAccount(cfg, "qwen-plan", prov, accountCred{APIKey: "good-key"}, "", true); err != nil {
		t.Fatalf("authful /models 200: %v", err)
	}
	if chatHits.Load() != 0 {
		t.Fatalf("non-ModelsAuthless provider was chat-probed %d times, want 0", chatHits.Load())
	}
}
