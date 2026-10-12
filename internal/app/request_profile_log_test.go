package app

// request_profile_log_test.go — composition coverage for the request-log
// profile field: a served request's committed record carries the routing
// profile (estimated tokens / image / tools / follow-up) and the conversation
// role composition, so the Requests detail answers "what filled this context
// window" without opening the body (magpie's context-grid insight, as a log
// field).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
)

func TestRequestProfileLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"a": {Provider: testProviderID, OpenAIBaseURL: upstream.URL}},
		Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "a", Model: "m"}}},
	}
	p, logDir, drain := newReqLogProxy(t, cfg)
	_ = p
	p.providers["a"] = &testProv{key: "ka"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)

	body := `{"model":"m","stream":false,"messages":[` +
		`{"role":"system","content":"be brief"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"hello"},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}]}`
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	awaitCommitMetrics(t, p, counters.PMKey{Provider: "a", Model: "m"})
	drain()
	records := allRecords(t, logDir)
	if len(records) == 0 {
		t.Fatal("no request-log records written")
	}
	rec := records[len(records)-1]
	if rec.Profile == nil {
		t.Fatalf("committed record carries no profile: %+v", rec)
	}
	if rec.Profile.EstimatedTokens <= 0 {
		t.Errorf("est_tokens = %d, want > 0", rec.Profile.EstimatedTokens)
	}
	if rec.Profile.Roles.System != 1 || rec.Profile.Roles.User != 2 ||
		rec.Profile.Roles.Assistant != 1 || rec.Profile.Roles.Tool != 1 {
		t.Errorf("role composition = %+v, want sys 1 / usr 2 / asst 1 / tool 1", rec.Profile.Roles)
	}
	// The body carries an assistant turn, so the profile classifies it as a
	// follow-up conversation turn (the routing FollowUp signal).
	if !rec.Profile.FollowUp {
		t.Errorf("assistant-carrying request not classified as follow-up: %+v", rec.Profile)
	}
	// A plain user-only follow-up turn is recognizable as one.
	body2 := `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`
	req2, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(body2))
	req2.Header.Set("content-type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	awaitCommitMetrics(t, p, counters.PMKey{Provider: "a", Model: "m"})
	drain()
	records = allRecords(t, logDir)
	rec2 := records[len(records)-1]
	if rec2.Profile == nil || !rec2.Profile.FollowUp || rec2.Profile.Roles.Assistant != 1 {
		t.Fatalf("follow-up profile = %+v", rec2.Profile)
	}
}
