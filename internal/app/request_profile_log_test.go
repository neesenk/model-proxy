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
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
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
	p.providers["a"] = &testProv{key: "ka"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)

	body := `{"model":"m","stream":false,"messages":[` +
		`{"role":"system","content":"be brief"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"hello"},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}]}`
	body2 := `{"model":"m","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`
	cases := []struct {
		body  string
		roles requestlog.RoleCounts
	}{
		{body, requestlog.RoleCounts{System: 1, User: 2, Assistant: 1, Tool: 1}},
		{body2, requestlog.RoleCounts{User: 2, Assistant: 1}},
	}
	for _, tc := range cases {
		if status, _ := post(t, px.URL+"/v1/chat/completions", tc.body); status != http.StatusOK {
			t.Fatalf("request status = %d, want 200", status)
		}
	}
	px.Close() // Both handlers must finish enqueueing before the writer stops.
	drain()
	records := allRecords(t, logDir)
	if len(records) != len(cases) {
		t.Fatalf("record count = %d, want one per request (%d)", len(records), len(cases))
	}
	for _, tc := range cases {
		rec := recordContaining(t, records, tc.body)
		if rec.Profile == nil {
			t.Fatalf("request %s has no profile", rec.RequestID)
		}
		if rec.Profile.EstimatedTokens <= 0 || !rec.Profile.FollowUp || rec.Profile.Roles != tc.roles {
			t.Errorf("request %s profile = %+v, want positive tokens, follow-up and roles %+v", rec.RequestID, rec.Profile, tc.roles)
		}
	}
}
