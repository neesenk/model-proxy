package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// memOAuthStore is an in-memory OAuthBlobStore fake: it records Save traffic
// so tests can assert refresh write-backs land on the SEAM (the pool slot),
// not on a file path.
type memOAuthStore struct {
	mu    sync.Mutex
	blob  []byte
	saves int
}

func (m *memOAuthStore) Load() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blob, nil
}

func (m *memOAuthStore) Save(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blob = b
	m.saves++
	return nil
}

func (m *memOAuthStore) Delete() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blob = nil
	return nil
}

// TestCodexOAuth_RefreshWritesBackThroughSeam is the pooled-account refresh
// regression: a CodexOAuthProvider bound to a blob store rotates its tokens
// through the SEAM — the fresh access/refresh tokens land in the store's blob
// (which the pool-backed seam routes into that account's own pool slot under
// the cross-process lock), never through a file path.
func TestCodexOAuth_RefreshWritesBackThroughSeam(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(72*time.Hour).Unix())))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "head." + payload + ".sig",
			"refresh_token": "rt-rotated",
			"id_token":      "id-new",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	seed := &CodexAuthFile{}
	seed.AuthMode = "chatgpt"
	seed.Tokens.AccessToken = "at-expired"
	seed.Tokens.RefreshToken = "rt-old"
	seed.Tokens.AccountID = "acct-1"
	seedBlob, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	store := &memOAuthStore{blob: seedBlob}

	p := NewCodexOAuthProviderWithStore("pool-slot (diagnostic name)", store)
	p.tokenURL = srv.URL
	if err := p.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if store.saves != 1 {
		t.Fatalf("seam saves = %d, want exactly 1", store.saves)
	}
	var af CodexAuthFile
	if err := json.Unmarshal(store.blob, &af); err != nil {
		t.Fatalf("seam blob unreadable: %v", err)
	}
	if af.Tokens.RefreshToken != "rt-rotated" || af.Tokens.AccessToken == "at-expired" || af.Tokens.IDToken != "id-new" {
		t.Fatalf("rotated tokens = %+v", af.Tokens)
	}
	// The cached token serves the next request without another store write.
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatalf("inject after refresh: %v", err)
	}
	if store.saves != 1 {
		t.Fatalf("inject must not re-save, saves = %d", store.saves)
	}
}

// TestAqpKeyProvider_MintsThroughSeam: the aqp mint reads the SSO cookie from
// the seam-bound blob (the account's own pool slot).
func TestAqpKeyProvider_MintsThroughSeam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "SSO_C=slot-cookie" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"retcode":0,"data":{"api_key":"minted","project_id":"p"}}`)
	}))
	defer srv.Close()

	blob := []byte(`{"account_id":"one@x.com","sso_session_cookie":"SSO_C=slot-cookie"}`)
	key := NewAqpKeyProviderWithStore(srv.URL, "pool-slot", &memOAuthStore{blob: blob})
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := key.Inject(req); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer minted" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestFileOAuthStore_MirrorsRefSemantics: the default file-backed seam keeps
// the historical credstore behavior — absent file loads as (nil, nil).
func TestFileOAuthStore_MirrorsRefSemantics(t *testing.T) {
	s := FileOAuthStore{Path: t.TempDir() + "/absent.json"}
	blob, err := s.Load()
	if err != nil || blob != nil {
		t.Fatalf("absent store = %s, %v; want (nil, nil)", blob, err)
	}
	if err := s.Save([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if blob, err := s.Load(); err != nil || string(blob) != `{}` {
		t.Fatalf("roundtrip = %s, %v", blob, err)
	}
	if err := s.Delete(); err != nil {
		t.Fatal(err)
	}
	if blob, err := s.Load(); err != nil || blob != nil {
		t.Fatalf("deleted store = %s, %v; want (nil, nil)", blob, err)
	}
}
