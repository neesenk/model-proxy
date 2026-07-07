package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// auth_gateway_test.go covers the testable surface of auth.go (the main-package
// AuthProvider implementations) + gateway.go (compass client helpers) +
// codex_login.go (defaults/dirOf) — all with temp cred files / mock httptest,
// no real network or browser.

// --- ApiKeyProvider: Inject reads key from file; Refresh clears cache ---

func TestApiKeyProvider_InjectAndRefresh(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "zhipu_apikey.json")
	os.WriteFile(cred, mustMarshalT(map[string]string{"api_key": "zk"}), 0o600)
	p := newApiKeyProvider(cred)

	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer zk" {
		t.Errorf("Inject Authorization=%q want Bearer zk", got)
	}
	if req.Header.Get("x-api-key") != "" {
		t.Error("x-api-key should be cleared by ApiKeyProvider")
	}
	// Cached: even if file is removed, Inject still returns the cached key.
	os.Remove(cred)
	req2, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req2); err != nil {
		t.Fatal(err)
	}
	if got := req2.Header.Get("Authorization"); got != "Bearer zk" {
		t.Errorf("cached Inject=%q want Bearer zk", got)
	}
	// Refresh clears cache → next Inject reads file (now gone) → error.
	if err := p.Refresh(); err != nil {
		t.Fatal(err)
	}
	req3, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req3); err == nil {
		t.Error("Inject after Refresh (file removed): want error, got nil")
	}
}

func TestApiKeyProvider_NotLoggedIn(t *testing.T) {
	p := newApiKeyProvider(filepath.Join(t.TempDir(), "missing.json"))
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err == nil {
		t.Error("Inject with no cred file: want error, got nil")
	}
}

func TestApiKeyProvider_EmptyKey(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "k.json")
	os.WriteFile(cred, mustMarshalT(map[string]string{"api_key": ""}), 0o600)
	p := newApiKeyProvider(cred)
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err == nil {
		t.Error("Inject with empty api_key: want error, got nil")
	}
}

func TestApiKeyProvider_BadJSON(t *testing.T) {
	dir := t.TempDir()
	cred := filepath.Join(dir, "k.json")
	os.WriteFile(cred, []byte(`not-json`), 0o600)
	p := newApiKeyProvider(cred)
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err == nil {
		t.Error("Inject with bad JSON: want error, got nil")
	}
}

// --- main-package StaticProvider ---

func TestMainStaticProvider(t *testing.T) {
	s := &StaticProvider{key: "sk"}
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := s.Inject(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk" {
		t.Errorf("static Inject=%q want Bearer sk", got)
	}
	if err := s.Refresh(); err != nil {
		t.Errorf("static Refresh: want nil, got %v", err)
	}
}

// --- newAuthProvider factory ---

func TestNewAuthProvider_Factory(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"compass":    {Provider: "compass", CQPMintURL: "https://x/mint"},
			"codex":      {Provider: "codex"},
			"zhipu":      {Provider: "zhipu"},
			"deepseek":   {Provider: "deepseek"},
			"volcengine": {Provider: "volcengine"},
			"static":     {Provider: "static"},
			"unknown":    {Provider: "nope"},
		},
	}
	for _, name := range []string{"compass", "codex", "zhipu", "deepseek", "volcengine", "static", "unknown"} {
		if p := newAuthProvider(cfg.Providers[name].Provider, name, cfg); p == nil {
			t.Errorf("newAuthProvider(%s): nil provider", name)
		}
	}
}

// --- CQPProvider: key minting via mock + caching + refresh ---

func TestCQPProvider_MintAndCache(t *testing.T) {
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Write([]byte(`{"retcode":0,"data":{"api_key":"cqp-key-12345","project_id":"pid","employee_email":"u@x"}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := filepath.Join(dir, "compass_oauth_auth.json")
	os.WriteFile(store, mustMarshalT(map[string]string{"sso_session_cookie": "SSO_C=abc"}), 0o600)

	p := newCQPProvider(srv.URL, store)
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer cqp-key-12345" {
		t.Errorf("CQP Inject=%q want Bearer cqp-key-12345", got)
	}
	// Second Inject uses cache (no second mint).
	req2, _ := http.NewRequest("GET", "https://x", nil)
	p.Inject(req2)
	if mints != 1 {
		t.Errorf("CQP mints=%d want 1 (cached)", mints)
	}
	// Refresh clears cache → next Inject re-mints.
	if err := p.Refresh(); err != nil {
		t.Fatal(err)
	}
	req3, _ := http.NewRequest("GET", "https://x", nil)
	p.Inject(req3)
	if mints != 2 {
		t.Errorf("CQP mints=%d want 2 (after refresh)", mints)
	}
}

func TestCQPProvider_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	store := filepath.Join(dir, "compass_oauth_auth.json")
	os.WriteFile(store, mustMarshalT(map[string]string{"sso_session_cookie": "SSO_C=abc"}), 0o600)
	p := newCQPProvider(srv.URL, store)
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err == nil {
		t.Error("CQP Inject on 500: want error, got nil")
	}
}

func TestCQPProvider_BadRetcode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"retcode":1,"message":"denied"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	store := filepath.Join(dir, "compass_oauth_auth.json")
	os.WriteFile(store, mustMarshalT(map[string]string{"sso_session_cookie": "SSO_C=abc"}), 0o600)
	p := newCQPProvider(srv.URL, store)
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err == nil || !strings.Contains(err.Error(), "retcode") {
		t.Errorf("CQP Inject bad retcode: err=%v want retcode error", err)
	}
}

func TestCQPProvider_NoSSOCookie(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "compass_oauth_auth.json") // absent
	p := newCQPProvider("https://x", store)
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.Inject(req); err == nil {
		t.Error("CQP Inject with no SSO cookie file: want error, got nil")
	}
}

// --- readSSOCookie ---

func TestReadSSOCookie(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "g.json")
	os.WriteFile(good, mustMarshalT(map[string]string{"sso_session_cookie": "SSO_C=x"}), 0o600)
	if got, err := readSSOCookie(good); err != nil || got != "SSO_C=x" {
		t.Errorf("readSSOCookie(good)=%q err=%v", got, err)
	}
	if _, err := readSSOCookie(filepath.Join(dir, "nope")); err == nil {
		t.Error("readSSOCookie(missing): want error")
	}
	empty := filepath.Join(dir, "empty.json")
	os.WriteFile(empty, mustMarshalT(map[string]string{"sso_session_cookie": ""}), 0o600)
	if _, err := readSSOCookie(empty); err == nil {
		t.Error("readSSOCookie(empty cookie): want error")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`not-json`), 0o600)
	if _, err := readSSOCookie(bad); err == nil {
		t.Error("readSSOCookie(bad json): want error")
	}
	if _, err := readSSOCookie(""); err == nil {
		t.Error("readSSOCookie(empty path): want error")
	}
}

// --- gateway: clearAccount ---

func TestClearAccount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "acct.json")
	os.WriteFile(p, []byte(`{}`), 0o600)
	if err := clearAccount(p); err != nil {
		t.Errorf("clearAccount existing: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("clearAccount did not remove the file")
	}
	// Idempotent: missing file is not an error.
	if err := clearAccount(p); err != nil {
		t.Errorf("clearAccount missing: want nil, got %v", err)
	}
}

// --- gateway: newCompassClient + GetManagedKey cache + InvalidateKey ---

func TestCompassClient_GetManagedKeyCache(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "compass_oauth_auth.json")
	os.WriteFile(store, mustMarshalT(map[string]string{"sso_session_cookie": "SSO_C=abc"}), 0o600)

	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Write([]byte(`{"retcode":0,"data":{"api_key":"managed-key","project_id":"pid"}}`))
	}))
	defer srv.Close()

	c := newCompassClient(store)
	// fetchAPIKeyAt (mock URL) populates cachedKey in memory.
	if _, err := c.fetchAPIKeyAt(srv.URL); err != nil {
		t.Fatalf("fetchAPIKeyAt: %v", err)
	}
	if mints != 1 {
		t.Fatalf("fetchAPIKeyAt mints=%d want 1", mints)
	}
	// GetManagedKey must hit the cache (no second mint, no real-URL call).
	k1, err := c.GetManagedKey()
	if err != nil || k1 != "managed-key" {
		t.Fatalf("GetManagedKey=%q err=%v want managed-key", k1, err)
	}
	if mints != 1 {
		t.Errorf("mints=%d want 1 (GetManagedKey should hit cache)", mints)
	}
	// Invalidate → next GetManagedKey re-fetches via the package URL, which we
	// can't redirect. Instead, verify Invalidate clears the cache by calling
	// fetchAPIKeyAt again (the production refresh path on 401).
	c.InvalidateKey()
	if c.cachedKey != "" {
		t.Errorf("InvalidateKey did not clear cachedKey=%q", c.cachedKey)
	}
	if _, err := c.fetchAPIKeyAt(srv.URL); err != nil {
		t.Fatalf("fetchAPIKeyAt after invalidate: %v", err)
	}
	if mints != 2 {
		t.Errorf("mints=%d want 2 (re-fetch after invalidate)", mints)
	}
}

// GetManagedKey with no cache and no SSO cookie errors (no real network).
func TestCompassClient_GetManagedKey_NoCookie(t *testing.T) {
	c := newCompassClient(filepath.Join(t.TempDir(), "missing.json"))
	if _, err := c.GetManagedKey(); err == nil {
		t.Error("GetManagedKey with no cookie/store: want error, got nil")
	}
}

// --- gateway: MonthlyUsage via mock ---

func TestCompassClient_MonthlyUsage(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "compass_oauth_auth.json")
	os.WriteFile(store, mustMarshalT(map[string]string{
		"sso_session_cookie": "SSO_C=abc",
		"project_id":         "pid",
	}), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify POST + project_id in body + cookie header.
		if r.Method != "POST" {
			t.Errorf("MonthlyUsage method=%q want POST", r.Method)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["project_id"] != "pid" {
			t.Errorf("MonthlyUsage body project_id=%q want pid", body["project_id"])
		}
		if r.Header.Get("Cookie") == "" {
			t.Error("MonthlyUsage missing Cookie header")
		}
		w.Write([]byte(`{"retcode":0,"data":{"project_id":"pid","selected_year":2026,"selected_month":7,"total_amount":100,"usage":30,"balance":70,"plan":"CQP"}}`))
	}))
	defer srv.Close()

	c := newCompassClient(store)
	// Override the monthly usage URL by swapping the package var via a copy:
	// MonthlyUsage() uses the package-level compassMonthlyUsage constant, so we
	// point the client at the mock by re-fetching at the mock URL directly.
	mu, err := c.monthlyUsageAt(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if mu.TotalAmount != 100 || mu.Balance != 70 || mu.Plan != "CQP" {
		t.Errorf("MonthlyUsage=%+v want 100/70/CQP", mu)
	}
}

func TestCompassClient_MonthlyUsage_NotLoggedIn(t *testing.T) {
	c := newCompassClient(filepath.Join(t.TempDir(), "missing.json"))
	if _, err := c.MonthlyUsage(); err == nil {
		t.Error("MonthlyUsage with no store: want error, got nil")
	}
}

// --- codex_login: defaults + dirOf ---

func TestCodexLoginOptions_Defaults(t *testing.T) {
	o := &codexLoginServerOptions{}
	o.defaults()
	if o.usercodeURL == "" || o.deviceTokURL == "" || o.tokenURL == "" {
		t.Errorf("defaults left empty URLs: %+v", o)
	}
	if o.httpClient == nil {
		t.Error("defaults left httpClient nil")
	}
	// Defaults must NOT overwrite already-set fields.
	o2 := &codexLoginServerOptions{usercodeURL: "custom", httpClient: &http.Client{Timeout: 1}}
	o2.defaults()
	if o2.usercodeURL != "custom" {
		t.Errorf("defaults overwrote usercodeURL: %q", o2.usercodeURL)
	}
}

func TestDirOf(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"/a/b/c.yaml", "/a/b"},
		{"c.yaml", "."},
		{"", "."},
		{"/x", ""},
	} {
		if got := dirOf(tc.in); got != tc.want {
			t.Errorf("dirOf(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

// mustMarshalT is the main-package twin of provider.mustMarshal.
func mustMarshalT(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ensure time import is used (MonthlyUsage signature uses time only indirectly).
var _ = time.Second
