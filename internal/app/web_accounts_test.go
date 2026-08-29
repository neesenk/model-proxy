package app

import (
	"encoding/base64"
	"encoding/json"
	cliframework "model-proxy/internal/cli/framework"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	"model-proxy/internal/provider"
)

func TestAccountsListMasked(t *testing.T) {
	setPoolHome(t, t.TempDir())
	if err := AccountStore().Save("zhipu", "zhipu", CredentialPool{
		Version: 1,
		Accounts: []PoolAccount{{
			// ID is derived from the credential (AccountID), like login does.
			ID:        accounts.AccountID("zhipu", AccountCred{APIKey: "sk-secret-key-1234567890"}),
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
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	var decoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode accounts response: %v: %s", err, body)
	}
	assertJSONKeysAbsent(t, decoded, map[string]bool{
		"api_key":            true,
		"access_key":         true,
		"secret_key":         true,
		"sso_session_cookie": true,
	})
	// No raw secret may appear anywhere in the response.
	for _, secret := range []string{"sk-secret-key-1234567890", "AK-LEAK-12345", "SK-LEAK-67890"} {
		if strings.Contains(body, secret) {
			t.Errorf("raw secret leaked (%s):\n%s", secret, body)
		}
	}
	// The real account id MUST be present (unmasked) — the UI sends it back on
	// remove, so masking it would break deletion.
	if want := accounts.AccountID("zhipu", AccountCred{APIKey: "sk-secret-key-1234567890"}); !strings.Contains(body, `"id":"`+want+`"`) {
		t.Errorf("real id (for removal) missing:\n%s", body)
	}
	if !strings.Contains(body, `"label":"work"`) {
		t.Errorf("label missing:\n%s", body)
	}
}

// TestAccountsListCodex is the regression test for the bug where the account
// tab showed codex as "No account configured" despite being logged in.
// handleAccountsList used LoadAqpAccount (the wrong on-disk shape) for codex,
// so every field mapped to empty and the AccountID guard dropped the entry.
// Now codex uses LoadCodexAccount (CodexAuthFile shape) and surfaces account_id
// + email parsed from the id_token. Asserts the real account_id + email are
// present AND no token (access/refresh/id) leaks.
func TestAccountsListCodex(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Build a codex auth file: a stored account_id, an id_token JWT carrying
	// email + chatgpt_account_id, and secret access/refresh tokens that must
	// never appear in the response.
	idPayload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"email":"coder@openai.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct-codex-42"}}`))
	af := provider.CodexAuthFile{AuthMode: "chatgpt"}
	af.Tokens.AccessToken = "atk-TOPSECRET-codex"
	af.Tokens.RefreshToken = "rtk-TOPSECRET-codex"
	af.Tokens.IDToken = "head." + idPayload + ".sig"
	af.Tokens.AccountID = "acct-codex-42"
	ab, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(cliframework.AuthFilePath("codex", "oauth_auth"), ab, 0o600); err != nil {
		t.Fatal(err)
	}

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	var decoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode codex accounts response: %v: %s", err, body)
	}
	assertJSONKeysAbsent(t, decoded, map[string]bool{
		"access_token":  true,
		"refresh_token": true,
		"id_token":      true,
	})
	// The account_id (used by the UI for removal) + email label must appear.
	if !strings.Contains(body, `"id":"acct-codex-42"`) {
		t.Errorf("account_id missing:\n%s", body)
	}
	if !strings.Contains(body, `"email":"coder@openai.com"`) {
		t.Errorf("email (label) missing:\n%s", body)
	}
	// No token may leak - the acct struct has no field for any of them.
	for _, secret := range []string{"atk-TOPSECRET-codex", "rtk-TOPSECRET-codex", idPayload} {
		if strings.Contains(body, secret) {
			t.Errorf("token leaked (%s):\n%s", secret, body)
		}
	}
	// The codex provider card must carry one account (not the empty state).
	var resp struct {
		Providers []struct {
			Name     string `json:"name"`
			Accounts []struct {
				ID string `json:"id"`
			} `json:"accounts"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v: %s", err, body)
	}
	var n int
	for _, pr := range resp.Providers {
		if pr.Name == "codex" {
			n = len(pr.Accounts)
		}
	}
	if n != 1 {
		t.Errorf("codex accounts=%d want 1 (was 0 before the fix)", n)
	}
}

func assertJSONKeysAbsent(t *testing.T, value any, forbidden map[string]bool) {
	t.Helper()
	var visit func(any)
	visit = func(current any) {
		switch current := current.(type) {
		case map[string]any:
			for key, child := range current {
				if forbidden[key] {
					t.Errorf("forbidden credential field %q present in accounts response", key)
				}
				visit(child)
			}
		case []any:
			for _, child := range current {
				visit(child)
			}
		}
	}
	visit(value)
}

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
		serveWeb(w, rec, httptest.NewRequest("POST", "/api/accounts/"+name,
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
	serveWeb(w, rec404, httptest.NewRequest("POST", "/api/accounts/nope",
		strings.NewReader(`{"api_key":"x"}`)))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("unknown provider add status=%d want 404", rec404.Code)
	}

	// apikey add: validate (200 from usage mock) → save to pool → reload (best-effort).
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/accounts/zhipu",
		strings.NewReader(`{"api_key":"sk-test-1234567890","label":"work"}`)))
	if rec.Code != 200 {
		t.Fatalf("add status=%d body=%s", rec.Code, rec.Body.String())
	}
	pool, _ := LoadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("account not added: %+v", pool.Accounts)
	}
	if pool.Accounts[0].Label != "work" {
		t.Errorf("label not saved: %q", pool.Accounts[0].Label)
	}
	id := pool.Accounts[0].ID

	// remove → pool emptied.
	rec2 := httptest.NewRecorder()
	serveWeb(w, rec2, httptest.NewRequest("DELETE", "/api/accounts/zhipu/"+id, nil))
	if rec2.Code != 200 {
		t.Fatalf("remove status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	pool2, _ := LoadPool("zhipu", "zhipu")
	if len(pool2.Accounts) != 0 {
		t.Fatalf("account not removed: %+v", pool2.Accounts)
	}

	// remove with a missing id segment → 400 (not a panic / 500).
	rec3 := httptest.NewRecorder()
	serveWeb(w, rec3, httptest.NewRequest("DELETE", "/api/accounts/zhipu", nil))
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("malformed remove status=%d want 400: %s", rec3.Code, rec3.Body.String())
	}

	// remove on unknown provider → 404 (route table intact).
	rec404b := httptest.NewRecorder()
	serveWeb(w, rec404b, httptest.NewRequest("DELETE", "/api/accounts/nope/x", nil))
	if rec404b.Code != http.StatusNotFound {
		t.Errorf("remove unknown provider status=%d want 404", rec404b.Code)
	}

	// add JSON decode failure → 400.
	recBad := httptest.NewRecorder()
	serveWeb(w, recBad, httptest.NewRequest("POST", "/api/accounts/zhipu",
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
	serveWeb(w, recVal, httptest.NewRequest("POST", "/api/accounts/zhipu",
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
	w.Register(mux)

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
