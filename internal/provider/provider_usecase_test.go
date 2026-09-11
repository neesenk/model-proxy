package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// provider_usecase_test.go covers each provider implementation's
// RewriteRequest / AuthHeaders / FetchModels / ensureJSONField through the
// behaviors a user depends on, plus the New() dispatch edge cases. Tests run
// in package provider (white-box) to reach unexported helpers.

// fakeAuth injects a Bearer key without touching the filesystem.
type fakeAuth struct{ key string }

func (a fakeAuth) Inject(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.key)
	return nil
}
func (a fakeAuth) Refresh() error { return nil }

// --- P1: aqp RewriteRequest adds ?beta=true to /messages ---

func TestAqpRewrite_AddsBetaToMessages(t *testing.T) {
	p := &AqpProvider{cfg: &Config{}, auth: fakeAuth{key: "k"}}
	for _, tc := range []struct {
		name     string
		url      string
		path     string
		wantBeta bool
	}{
		{"messages no query", "https://x/messages", "/v1/messages", true},
		{"messages with query", "https://x/messages?stream=true", "/v1/messages", true},
		{"responses not messages", "https://x/responses", "/v1/responses", false},
		{"already has beta", "https://x/messages?beta=true", "/v1/messages", true}, // not double-added
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, _ := p.RewriteRequest(tc.url, []byte(`{}`), tc.path)
			hasBeta := strings.Contains(gotURL, "beta=true")
			if hasBeta != tc.wantBeta {
				t.Errorf("url=%q beta=true present=%v want %v", gotURL, hasBeta, tc.wantBeta)
			}
			// Ensure beta is not added twice.
			if strings.Count(gotURL, "beta=true") > 1 {
				t.Errorf("url=%q has beta=true more than once (double-added)", gotURL)
			}
		})
	}
}

// --- P2: aqp AuthHeaders delegates to cfg.Auth ---

func TestAqpAuthHeaders_Delegates(t *testing.T) {
	p := &AqpProvider{cfg: &Config{}, auth: fakeAuth{key: "ck"}}
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ck" {
		t.Errorf("Authorization=%q want Bearer ck", got)
	}
}

// --- P3: codex RewriteRequest injects store:false when absent ---

func TestCodexRewrite_InjectsStoreFalse(t *testing.T) {
	p := &CodexProvider{cfg: &Config{}, auth: fakeAuth{key: "k"}}
	_, body := p.RewriteRequest("https://x/responses", []byte(`{"model":"gpt-5.5"}`), "/v1/responses")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	if v, ok := m["store"]; !ok || v != false {
		t.Errorf("store=%v ok=%v want false/present", v, ok)
	}
}

// --- P4: codex RewriteRequest leaves an explicit store:true untouched ---

func TestCodexRewrite_RespectsExistingStore(t *testing.T) {
	p := &CodexProvider{cfg: &Config{}, auth: fakeAuth{key: "k"}}
	_, body := p.RewriteRequest("https://x", []byte(`{"model":"gpt-5.5","store":true}`), "/v1/responses")
	var m map[string]any
	json.Unmarshal(body, &m)
	if v, _ := m["store"].(bool); v != true {
		t.Errorf("store=%v want true (explicit value must not be overwritten)", v)
	}
}

// --- P5: codex ensureJSONField on non-JSON body returns it unchanged ---

func TestEnsureJSONField_NonJSON(t *testing.T) {
	orig := []byte(`not-json`)
	got := ensureJSONField(orig, "store", false)
	if string(got) != string(orig) {
		t.Errorf("ensureJSONField on non-JSON: got %q want %q", got, orig)
	}
}

func TestEnsureJSONField_InjectsMissingAndPreservesExisting(t *testing.T) {
	missing := ensureJSONField([]byte(`{"model":"gpt-5.5"}`), "store", false)
	var got map[string]any
	if err := json.Unmarshal(missing, &got); err != nil {
		t.Fatalf("injected body is not JSON: %s", missing)
	}
	if got["store"] != false {
		t.Errorf("injected store=%v want false", got["store"])
	}

	existing := []byte(`{"store":true,"model":"gpt-5.5"}`)
	if got := ensureJSONField(existing, "store", false); string(got) != string(existing) {
		t.Errorf("existing store must remain byte-for-byte untouched: got %q want %q", got, existing)
	}
}

// --- P6: codex FetchModels queries /models live and keeps visibility=="list" ---

func TestCodexFetchModels_LiveQuery(t *testing.T) {
	var gotURL, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"models":[
			{"slug":"gpt-5.6-sol","visibility":"list"},
			{"slug":"gpt-5.5","visibility":"list"},
			{"slug":"codex-auto-review","visibility":"hide"},
			{"slug":"gpt-5.4","visibility":"none"}
		]}`))
	}))
	defer srv.Close()

	p := &CodexProvider{cfg: &Config{
		OpenAIBaseURL: srv.URL + "/backend-api/codex",
		ClientVersion: "0.144.1",
	}, auth: fakeAuth{key: "tok-abc"}}
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	// Server order preserved; only visibility=="list" slugs kept.
	if len(ids) != 2 || ids[0] != "gpt-5.6-sol" || ids[1] != "gpt-5.5" {
		t.Errorf("ids: got %v want [gpt-5.6-sol gpt-5.5]", ids)
	}
	if gotURL != "/backend-api/codex/models?client_version=0.144.1" {
		t.Errorf("URL: got %q, want /backend-api/codex/models?client_version=0.144.1", gotURL)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Errorf("Authorization: got %q, want Bearer tok-abc", gotAuth)
	}
}

// --- P6b: codex FetchModels surfaces non-200 as an error (no silent fallback) ---

func TestCodexFetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{
		OpenAIBaseURL: srv.URL + "/codex",
		ClientVersion: "0.144.1",
	}, auth: fakeAuth{key: "k"}}
	_, err := p.FetchModels()
	if err == nil {
		t.Fatal("expected error on HTTP 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention status 403, got %q", err.Error())
	}
}

// --- P7: apikey AuthHeaders reads key from file + Bearer injection ---

func TestApiKeyBase_AuthHeadersFromFile(t *testing.T) {
	dir := t.TempDir()
	authFile := filepath.Join(dir, "test_apibase.json")
	keyJSON, _ := json.Marshal(map[string]string{"api_key": "file-key"})
	if err := os.WriteFile(authFile, keyJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	b := &ApiKeyBase{authFile: authFile}
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := b.AuthHeaders(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer file-key" {
		t.Errorf("Authorization=%q want Bearer file-key", got)
	}
	if req.Header.Get("x-api-key") != "" {
		t.Error("x-api-key should be deleted by ApiKeyBase (Bearer-only)")
	}
}

// --- P8: apikey AuthHeaders errors when not logged in ---

func TestApiKeyBase_NotLoggedIn(t *testing.T) {
	b := &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "missing.json")}
	req, _ := http.NewRequest("GET", "https://x", nil)
	err := b.AuthHeaders(req)
	if err == nil {
		t.Error("AuthHeaders on missing auth file: want error, got nil")
	}
}

// --- P9: apikey SaveKey → LoadKey round-trip + caching ---

func TestApiKeyBase_SaveLoadCache(t *testing.T) {
	dir := t.TempDir()
	b := &ApiKeyBase{authFile: filepath.Join(dir, "k.json")}
	if err := b.SaveKey("my-key"); err != nil {
		t.Fatal(err)
	}
	got, err := b.LoadKey()
	if err != nil || got != "my-key" {
		t.Fatalf("LoadKey after SaveKey: got %q err=%v want my-key", got, err)
	}
	// Delete the file; cached value should still be returned.
	os.Remove(b.authFile)
	got, err = b.LoadKey()
	if err != nil || got != "my-key" {
		t.Errorf("LoadKey after delete (cached): got %q err=%v want my-key (cached)", got, err)
	}
	// Refresh clears cache; next LoadKey must error (file gone).
	if err := b.Refresh(); err != nil {
		t.Fatal(err)
	}
	_, err = b.LoadKey()
	if err == nil {
		t.Error("LoadKey after Refresh (file deleted): want error, got nil")
	}
}

// --- P10: apikey DeleteKey is idempotent ---

func TestApiKeyBase_DeleteKeyIdempotent(t *testing.T) {
	b := &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "k.json")}
	// Deleting a non-existent file must not error.
	if err := b.DeleteKey(); err != nil {
		t.Errorf("DeleteKey on missing file: want nil, got %v", err)
	}
}

// --- P11: deepseek AuthHeaders sets BOTH Bearer and x-api-key ---

func TestDeepSeekAuthHeaders_DualScheme(t *testing.T) {
	dir := t.TempDir()
	authFile := filepath.Join(dir, "ds.json")
	os.WriteFile(authFile, mustMarshal(map[string]string{"api_key": "ds-key"}), 0o600)
	p := &DeepSeekProvider{
		ApiKeyBase: &ApiKeyBase{authFile: authFile},
		cfg:        &Config{},
	}
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ds-key" {
		t.Errorf("Authorization=%q want Bearer ds-key", got)
	}
	if got := req.Header.Get("x-api-key"); got != "ds-key" {
		t.Errorf("x-api-key=%q want ds-key (deepseek dual-auth)", got)
	}
}

// --- P12: volcengine AuthHeaders sets BOTH Bearer and x-api-key ---

func TestVolcengineAuthHeaders_DualScheme(t *testing.T) {
	dir := t.TempDir()
	authFile := filepath.Join(dir, "vol.json")
	os.WriteFile(authFile, mustMarshal(map[string]string{"api_key": "vol-key"}), 0o600)
	p := &VolcengineProvider{
		ApiKeyBase: &ApiKeyBase{authFile: authFile},
		cfg:        &Config{},
	}
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer vol-key" {
		t.Errorf("Authorization=%q want Bearer vol-key", got)
	}
	if got := req.Header.Get("x-api-key"); got != "vol-key" {
		t.Errorf("x-api-key=%q want vol-key (volcengine dual-auth)", got)
	}
}

// --- P13: zhipu FetchModels hits the configured /models endpoint ---

func TestZhipuFetchModels_FromEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer zk" {
			t.Errorf("FetchModels auth=%q want Bearer zk", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model"},{"id":"glm-5.1","object":"model"}]}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{
		ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "zk"),
		cfg:        &Config{OpenAIBaseURL: srv.URL},
	}
	got, err := p.FetchModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "glm-5.2" || got[1] != "glm-5.1" {
		t.Errorf("FetchModels()=%v want [glm-5.2 glm-5.1]", got)
	}
}

// --- P14: New() errors on unknown provider_id ---

func TestNew_UnknownProviderID(t *testing.T) {
	_, err := New(&Config{ProviderID: "nope"}, "nope")
	if err == nil {
		t.Fatal("New with unknown provider_id: want error, got nil")
	}
}

// --- P15: StaticProvider returns errNotSupported for Usage/FetchModels ---

func TestStaticProvider_NotSupported(t *testing.T) {
	p := &StaticProvider{cfg: &Config{}}
	if err := p.Usage(); err == nil {
		t.Error("StaticProvider.Usage: want error, got nil")
	}
	if _, err := p.FetchModels(); err == nil {
		t.Error("StaticProvider.FetchModels: want error, got nil")
	}
}

// --- P16: QuotaSnapshot.Surplus returns 0 for non-plan / nil ---

func TestSurplus_NilAndNonPlan(t *testing.T) {
	if got := (*QuotaSnapshot)(nil).Surplus(time.Now(), 1); got != 0 {
		t.Errorf("nil Surplus=%v want 0", got)
	}
	s := &QuotaSnapshot{Billing: BillingUnknown, RemainingPct: 0.5}
	if got := s.Surplus(time.Now(), 1); got != 0 {
		t.Errorf("unknown-billing Surplus=%v want 0", got)
	}
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// --- P17: codex/deepseek Logout/Surplus (Login was removed from the interface;
// Usage()/Quota() are direct impls exercised by the fetch/display tests) ---

func TestProviderDelegates_Callbacks(t *testing.T) {
	cfg := &Config{}
	// codex: Logout is provider-owned (file removal, Phase 5); Usage()/Quota()
	// are direct impls - exercised by the fetch/display tests, not here.
	codex := &CodexProvider{cfg: cfg}
	mustNoErr(t, codex.Logout())
}

// --- P18: deleted in Phase 2 (QuotaOrUnknown/QuotaFn removed; each provider
// implements Quota() directly - see TestStaticProviderQuota_Unknown in
// quota_test.go for the BillingUnknown path). ---

// --- P19: NewApiKeyBase + AuthFilePath ---

func TestNewApiKeyBase_Path(t *testing.T) {
	b := NewApiKeyBase("zhipu-work")
	if !strings.HasSuffix(b.authFile, "zhipu-work_apikey.json") {
		t.Errorf("NewApiKeyBase authFile=%q want suffix zhipu-work_apikey.json", b.authFile)
	}
	if !strings.Contains(b.AuthFilePath(), ".model-proxy") {
		t.Errorf("AuthFilePath=%q want .model-proxy dir", b.AuthFilePath())
	}
}

// --- P20: fetchModelsBearer error paths (non-200, parse error) ---

func TestFetchModelsBearer_Errors(t *testing.T) {
	// Non-200 → error mentioning HTTP code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()
	_, err := fetchModelsBearer(&Config{OpenAIBaseURL: srv.URL}, fakeAuth{key: "k"}.Inject)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("fetchModelsBearer 401: err=%v want HTTP 401", err)
	}

	// 200 but non-JSON → parse error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not-json`))
	}))
	defer srv2.Close()
	_, err = fetchModelsBearer(&Config{OpenAIBaseURL: srv2.URL}, fakeAuth{key: "k"}.Inject)
	if err == nil {
		t.Error("fetchModelsBearer non-JSON: want parse error, got nil")
	}
}

// --- P21: truncateStr ---

func TestTruncateStr(t *testing.T) {
	if got := truncateStr("short", 10); got != "short" {
		t.Errorf("truncateStr(short)=%q want short", got)
	}
	if got := truncateStr("abcdef", 3); got != "abc..." {
		t.Errorf("truncateStr(abcdef,3)=%q want abc...", got)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// --- P22: static provider full surface ---

func TestStaticProvider_FullSurface(t *testing.T) {
	p := &StaticProvider{cfg: &Config{BoundAPIKey: "sk"}}
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk" {
		t.Errorf("static AuthHeaders=%q want Bearer sk", got)
	}
	if err := p.Refresh(); err != nil {
		t.Errorf("static Refresh: want nil, got %v", err)
	}
	if url, body := p.RewriteRequest("https://x/m", []byte(`b`), "/m"); url != "https://x/m" || string(body) != "b" {
		t.Errorf("static RewriteRequest=(%q,%q) want passthrough", url, body)
	}
	if err := p.Logout(); err == nil {
		t.Error("static Logout: want errNotSupported, got nil")
	}
	// Quota -> BillingUnknown (static provider has no measurable quota).
	q, err := p.Quota()
	if err != nil || q.Billing != BillingUnknown {
		t.Errorf("static Quota=%v err=%v want BillingUnknown", q, err)
	}
	// errNotSupported.Error() string.
	if e := (&notSupportedErr{}).Error(); e == "" {
		t.Error("notSupportedErr.Error() empty")
	}
}

// --- P23: codex AuthHeaders/Refresh delegate to cfg.Auth ---

func TestCodexAuthRefresh_Delegate(t *testing.T) {
	auth := &countingAuth{}
	p := &CodexProvider{cfg: &Config{}, auth: auth}
	req, _ := http.NewRequest("GET", "https://x", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatal(err)
	}
	if auth.injects != 1 {
		t.Errorf("codex AuthHeaders injects=%d want 1", auth.injects)
	}
	if err := p.Refresh(); err != nil {
		t.Fatal(err)
	}
	if auth.refreshes != 1 {
		t.Errorf("codex Refresh refreshes=%d want 1", auth.refreshes)
	}
}

// --- P24: aqp Refresh/Logout/Surplus delegation (Login removed from interface;
// Usage()/Quota() are direct implementations, network-tested elsewhere) ---

func TestAqpProvider_Delegates(t *testing.T) {
	cfg := &Config{}
	p := &AqpProvider{cfg: cfg, auth: fakeAuth{key: "k"}}
	mustNoErr(t, p.Refresh())
	mustNoErr(t, p.Logout())
}

// --- P25: deepseek/zhipu Logout/Quota/Surplus (Login removed from interface) ---

func TestDeepSeekProvider_Delegates(t *testing.T) {
	cfg := &Config{}
	dir := t.TempDir()
	authFile := filepath.Join(dir, "ds.json")
	os.WriteFile(authFile, mustMarshal(map[string]string{"api_key": "k"}), 0o600)
	p := &DeepSeekProvider{ApiKeyBase: &ApiKeyBase{authFile: authFile}, cfg: cfg}
	mustNoErr(t, p.Logout())
	// zhipu RewriteRequest is a no-op passthrough.
	zp := &ZhipuProvider{ApiKeyBase: &ApiKeyBase{authFile: authFile}, cfg: cfg}
	if url, body := zp.RewriteRequest("https://x/m", []byte(`b`), "/m"); url != "https://x/m" || string(body) != "b" {
		t.Errorf("zhipu RewriteRequest=(%q,%q) want passthrough", url, body)
	}
	mustNoErr(t, zp.Logout())
	if q, err := zp.Quota(); err != nil || q.Billing != BillingUnknown {
		t.Errorf("zhipu Quota (nil fn)=%v err=%v", q, err)
	}
}

// --- P26: volcengine delegation ---

func TestVolcengineProvider_Delegates(t *testing.T) {
	fetchCalled := false
	cfg := &Config{
		FetchModelsFn: func(context.Context) ([]string, error) { fetchCalled = true; return []string{"m"}, nil },
	}
	dir := t.TempDir()
	authFile := filepath.Join(dir, "v.json")
	os.WriteFile(authFile, mustMarshal(map[string]string{"api_key": "k"}), 0o600)
	p := &VolcengineProvider{ApiKeyBase: &ApiKeyBase{authFile: authFile}, cfg: cfg}
	mustNoErr(t, p.Logout())
	got, err := p.FetchModels()
	if err != nil || !fetchCalled || len(got) != 1 {
		t.Errorf("volcengine FetchModels got=%v err=%v called=%v", got, err, fetchCalled)
	}
	if q, err := p.Quota(); err != nil || q.Billing != BillingUnknown {
		t.Errorf("volcengine Quota=%v err=%v", q, err)
	}
}

// --- P27: top-level AuthFilePath deleted — it returned a literal "~/..." path
// that Go never expands, had no production callers (credential paths come from
// internal/accounts.AuthFilePath via the CLI framework), and duplicated that
// helper. ---

// countingAuth is a fakeAuth that records calls.
type countingAuth struct {
	injects, refreshes int
}

func (a *countingAuth) Inject(req *http.Request) error {
	a.injects++
	req.Header.Set("Authorization", "Bearer x")
	return nil
}
func (a *countingAuth) Refresh() error {
	a.refreshes++
	return nil
}

// --- P1: codex RewriteRequest merges include:["reasoning.encrypted_content"] ---
// The codex backend is stateless (store:false); without the encrypted_content
// include, multi-turn reasoning state is lost (cc-switch transform_responses).
func TestCodexRewriteRequest_IncludeEncryptedContent(t *testing.T) {
	p := &CodexProvider{}
	// Absent include → created with the marker.
	_, body := p.RewriteRequest("https://x/responses", []byte(`{"model":"gpt-5.5"}`), "/v1/responses")
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	inc, _ := m["include"].([]any)
	if len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v, want [reasoning.encrypted_content]", m["include"])
	}
	// Existing include without the marker → appended, existing entries kept.
	_, body2 := p.RewriteRequest("https://x/responses", []byte(`{"include":["code_interpreter.outputs"]}`), "/v1/responses")
	var m2 map[string]any
	if err := json.Unmarshal(body2, &m2); err != nil {
		t.Fatal(err)
	}
	inc2, _ := m2["include"].([]any)
	if len(inc2) != 2 || inc2[0] != "code_interpreter.outputs" || inc2[1] != "reasoning.encrypted_content" {
		t.Errorf("include = %v, want existing + marker appended", m2["include"])
	}
	// Marker already present → no duplicate.
	_, body3 := p.RewriteRequest("https://x/responses", []byte(`{"include":["reasoning.encrypted_content"]}`), "/v1/responses")
	var m3 map[string]any
	if err := json.Unmarshal(body3, &m3); err != nil {
		t.Fatal(err)
	}
	inc3, _ := m3["include"].([]any)
	if len(inc3) != 1 {
		t.Errorf("include = %v, want no duplicate", m3["include"])
	}
}
