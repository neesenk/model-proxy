package requestlog

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
)

func TestSessionIDFirstNonEmptyWins(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("x-claude-code-session-id", "")
	r.Header.Set("x-session-affinity", "sess-1")
	r.Header.Set("x-session-id", "  sess-2  ")
	if got := SessionID(r, configdomain.DefaultSessionHeaders); got != "sess-1" {
		t.Fatalf("session id = %q, want sess-1 (affinity outranks x-session-id)", got)
	}
	if got := SessionID(httptest.NewRequest(http.MethodPost, "/", nil), configdomain.DefaultSessionHeaders); got != "" {
		t.Fatalf("no header = %q, want empty", got)
	}
	// A custom allowlist overrides the order and trims the value.
	if got := SessionID(r, []string{"x-session-id"}); got != "sess-2" {
		t.Fatalf("custom allowlist = %q, want trimmed sess-2", got)
	}
}

// TestSessionIDFromBody pins the body-carried fallback for agents without
// session headers (Codex on the Responses protocol): extraction from
// client_metadata.session_id, and the defensive rejections.
func TestSessionIDFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "codex responses shape",
			body: `{"model":"gpt-5.6","stream":true,"client_metadata":{"session_id":" 01a0b26d-fde4-4f6f-8bd5-58cb9a0f8b1e ","thread_id":"t-1"},"input":[]}`,
			want: "01a0b26d-fde4-4f6f-8bd5-58cb9a0f8b1e",
		},
		{name: "no client_metadata", body: `{"model":"m","input":[]}`, want: ""},
		{name: "empty session_id", body: `{"client_metadata":{"session_id":""}}`, want: ""},
		{name: "non-string session_id", body: `{"client_metadata":{"session_id":42}}`, want: ""},
		{name: "oversized ignored", body: `{"client_metadata":{"session_id":"` + strings.Repeat("a", 129) + `"}}`, want: ""},
		{name: "invalid json", body: `{"client_metadata":`, want: ""},
		{name: "needle-less body skips parse", body: `not json at all`, want: ""},
	}
	for _, tc := range cases {
		if got := SessionIDFromBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: SessionIDFromBody = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBuildInputCarriesAgent(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	in := BuildInput(
		LogCtx{RequestID: "r1", Agent: "codex"},
		r, "anthropic", "m", configdomain.RouteTarget{}, resp, time.Now(), nil,
	)
	if in.Agent != "codex" {
		t.Errorf("agent = %q, want codex", in.Agent)
	}
}

func TestBuildInputSessionIDPrefersLogCtx(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("x-claude-code-session-id", "from-header")
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	in := BuildInput(
		LogCtx{RequestID: "r1", SessionID: "from-ctx"},
		r, "anthropic", "m", configdomain.RouteTarget{}, resp, time.Now(), nil,
	)
	if in.SessionID != "from-ctx" {
		t.Errorf("session id = %q, want from-ctx", in.SessionID)
	}
	// An empty LogCtx.SessionID keeps the legacy header fallback for callers
	// that predate the allowlist.
	in2 := BuildInput(
		LogCtx{RequestID: "r2"},
		r, "anthropic", "m", configdomain.RouteTarget{}, resp, time.Now(), nil,
	)
	if in2.SessionID != "from-header" {
		t.Errorf("legacy fallback session id = %q, want from-header", in2.SessionID)
	}
}
