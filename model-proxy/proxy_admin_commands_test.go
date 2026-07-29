package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-proxy/provider"
)

func TestWebAccountProbeUsesAdminCapabilityAndPreservesResponseShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const accountID = "account-one"
	if err := savePool("up", "static", credentialPool{Accounts: []poolAccount{{
		ID:     accountID,
		APIKey: "test-key",
	}}}); err != nil {
		t.Fatal(err)
	}

	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		gotModel = body.Model
		if got := r.Header.Get("Authorization"); got != "Bearer TEST" {
			t.Errorf("Authorization = %q, want exact fake-provider header", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"up": {
				Provider:      testProviderID,
				OpenAIBaseURL: upstream.URL,
				Models:        []string{"configured-model"},
			},
		},
	})
	p.mu.Lock()
	p.providers = map[string]provider.Provider{"up": &fakeProviderImpl{}}
	p.mu.Unlock()

	w := newWebServer(p, "test-config.yaml")
	rec := httptest.NewRecorder()
	w.serve(rec, httptest.NewRequest(
		http.MethodPost,
		"/api/accounts/up/"+accountID+"/test",
		strings.NewReader(""),
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Status     string `json:"status"`
		HTTPStatus int    `json:"http_status"`
		Provider   string `json:"provider"`
		AccountID  string `json:"account_id"`
		Model      string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.HTTPStatus != http.StatusOK ||
		got.Provider != "up" || got.AccountID != accountID ||
		got.Model != "configured-model" || gotModel != "configured-model" {
		t.Fatalf("probe response/upstream mismatch: response=%+v upstreamModel=%q", got, gotModel)
	}
}
