package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTypeSafeForTest(t *testing.T, cfg *Config) *TypeSafeProvider {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.BoundAPIKey == "" {
		cfg.BoundAPIKey = "sk-ts-testkey1234567890"
	}
	cfg.ProviderID = "typesafe"
	p, err := New(cfg, "typesafe")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*TypeSafeProvider)
}

func TestTypeSafe_AuthHeaders_Bearer(t *testing.T) {
	p := newTypeSafeForTest(t, nil)
	req := httptest.NewRequest(http.MethodPost, "https://x/v1/systemone", nil)
	req.Header.Set("x-api-key", "stray")
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-ts-testkey1234567890" {
		t.Errorf("Authorization = %q, want Bearer <key>", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key must be stripped, got %q", got)
	}
}

func TestTypeSafe_FetchModels_UsesDecisionsBase(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[{"id":"jev-1.13.0"},{"id":"jev-latest"}]}`))
	}))
	defer srv.Close()
	// decisions_base_url wins; openai_base_url (if any) must NOT be consulted.
	p := newTypeSafeForTest(t, &Config{
		OpenAIBaseURL:    "http://openai-base.invalid",
		DecisionsBaseURL: srv.URL + "/v1",
	})
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-ts-testkey1234567890" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if len(ids) != 2 || ids[0] != "jev-1.13.0" || ids[1] != "jev-latest" {
		t.Errorf("ids = %v", ids)
	}
}

func TestTypeSafe_FetchModels_OpenAIBaseFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"typesafe/jev-1.13.0"}]}`))
	}))
	defer srv.Close()
	p := newTypeSafeForTest(t, &Config{OpenAIBaseURL: srv.URL + "/api/v1"})
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(ids) != 1 || ids[0] != "typesafe/jev-1.13.0" {
		t.Errorf("ids = %v", ids)
	}
}

func TestTypeSafe_FetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	p := newTypeSafeForTest(t, &Config{DecisionsBaseURL: srv.URL})
	if _, err := p.FetchModels(); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want 401 surfaced", err)
	}
}

func TestTypeSafe_ProbeRequest_SystemOneShape(t *testing.T) {
	p := newTypeSafeForTest(t, nil)
	pr := p.ProbeRequest("jev-1.13.0")
	if pr.Method != http.MethodPost || pr.Path != "/systemone" {
		t.Errorf("probe = %s %s, want POST /systemone", pr.Method, pr.Path)
	}
	body := string(pr.Body)
	for _, want := range []string{`"model":"jev-1.13.0"`, `"state":"ping"`, `"type":"noul"`} {
		if !strings.Contains(body, want) {
			t.Errorf("probe body missing %s: %s", want, body)
		}
	}
}

// TypeSafe's /v1/models advertises the short family id (jev-1.13) which
// /systemone rejects — the filter rewrites it to the callable jev-X.Y.0 form.
func TestTypeSafe_FilterModelIDs_RewritesShortFamilyID(t *testing.T) {
	p := newTypeSafeForTest(t, nil)
	kept, dropped := p.FilterModelIDs([]string{"jev-1.13", "jev-latest", "jev-1.13.0", "other-model"})
	want := []string{"jev-1.13.0", "jev-latest", "jev-1.13.0", "other-model"}
	if len(kept) != len(want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Errorf("kept[%d] = %q, want %q", i, kept[i], want[i])
		}
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "jev-1.13") {
		t.Errorf("dropped = %v, want the short id visible", dropped)
	}
}

func TestTypeSafe_Quota_BillingUnknown(t *testing.T) {
	p := newTypeSafeForTest(t, nil)
	s, err := p.Quota()
	if err != nil || s == nil {
		t.Fatalf("Quota: %v %+v", err, s)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown (no public billing API)", s.Billing)
	}
}

// A pooled (bound) virtual must never touch the auth file: Logout/Refresh are
// no-ops and the bound key is the only credential used.
func TestTypeSafe_BoundKeyIsolation(t *testing.T) {
	p := newTypeSafeForTest(t, &Config{BoundAPIKey: "sk-ts-bound", DecisionsBaseURL: "http://x"})
	if err := p.Logout(); err != nil {
		t.Fatalf("Logout (bound no-op): %v", err)
	}
	key, err := p.LoadKey()
	if err != nil || key != "sk-ts-bound" {
		t.Fatalf("LoadKey = %q, %v after bound logout", key, err)
	}
}
