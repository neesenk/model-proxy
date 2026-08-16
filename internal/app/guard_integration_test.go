package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"model-proxy/internal/observe/counters"
)

// Outbound secret guard (DLP-lite) integration: the scan runs once on the
// shared request body inside forward, before the cache lookup and any forward
// branch. All fixtures are synthetic strings shaped to match the patterns —
// never real credentials (AGENTS.md credential red line), and failure messages
// must not echo the fixture bytes either.

// guardFixtureKey is a synthetic AWS-shaped access key id (all zeros).
const guardFixtureKey = "AKIA" + "0000000000000000"

func guardRequestBody() string {
	return `{"model":"glm","messages":[{"role":"user","content":"here is my key ` + guardFixtureKey + `"}]}`
}

// newGuardTestProxy wires a one-route proxy (guard.secrets = secretsAction)
// in front of a raw-body-capturing upstream.
func newGuardTestProxy(t *testing.T, secretsAction string) (p *Proxy, proxyURL string, upstreamBodies func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	cfg := &Config{
		Providers: map[string]Provider{
			"static": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "static", Model: "glm"}},
		},
		Guard: GuardConfig{Secrets: secretsAction},
	}
	p = newProxyWithStatic(t, cfg, map[string]string{"static": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)
	return p, px.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// guardEventDetails returns the Detail of every published "guard" event.
func guardEventDetails(p *Proxy) []string {
	var details []string
	for _, e := range p.events.Snapshot() {
		if e.Type == "guard" {
			details = append(details, e.Detail)
		}
	}
	return details
}

// Default (guard section absent) = log: forward the body unchanged, publish a
// guard event carrying only pattern type names, and count the hit.
func TestGuard_LogDefaultAllowsAndPublishesEvent(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "")

	postOK(t, proxyURL+"/v1/chat/completions", guardRequestBody())

	got := bodies()
	if len(got) != 1 || !strings.Contains(got[0], guardFixtureKey) {
		t.Fatalf("log action must forward the unmodified body once (calls=%d)", len(got))
	}
	details := guardEventDetails(p)
	if len(details) != 1 {
		t.Fatalf("guard events = %v, want exactly 1", details)
	}
	if !strings.Contains(details[0], "aws_access_key_id") || !strings.Contains(details[0], "action=log") {
		t.Errorf("guard event detail = %q, want pattern name + action=log", details[0])
	}
	if strings.Contains(details[0], guardFixtureKey) {
		t.Errorf("guard event leaked matched content")
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "aws_access_key_id"}].Requests; n != 1 {
		t.Errorf("guard hit counter = %d, want 1", n)
	}
}

func TestGuard_RedactRewritesBodyBeforeForwarding(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "redact")

	postOK(t, proxyURL+"/v1/chat/completions", guardRequestBody())

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(got))
	}
	if strings.Contains(got[0], guardFixtureKey) {
		t.Errorf("redacted body still contains the secret")
	}
	if !strings.Contains(got[0], "[REDACTED]") {
		t.Errorf("redacted body lacks the [REDACTED] placeholder")
	}
	// The rest of the JSON must survive (model field intact).
	if !strings.Contains(got[0], `"model":"glm"`) {
		t.Errorf("redacted body lost surrounding JSON content")
	}
	details := guardEventDetails(p)
	if len(details) != 1 || !strings.Contains(details[0], "action=redact") {
		t.Errorf("guard events = %v, want 1 event with action=redact", details)
	}
}

func TestGuard_BlockRejectsWith400(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "block")

	code, respBody := post(t, proxyURL+"/v1/chat/completions", guardRequestBody())
	if code != http.StatusBadRequest {
		t.Fatalf("block: status=%d body=%s, want 400", code, respBody)
	}
	if !strings.Contains(respBody, "aws_access_key_id") || !strings.Contains(respBody, "guard.secrets=block") {
		t.Errorf("block response = %q, want pattern name + reason", respBody)
	}
	if strings.Contains(respBody, guardFixtureKey) {
		t.Errorf("block response leaked matched content")
	}
	if got := bodies(); len(got) != 0 {
		t.Errorf("blocked request reached the upstream %d times, want 0", len(got))
	}
	// A blocked request is an early terminal: the live monitor needs its end
	// event pair (same contract as the other 400 terminals).
	var sawEnd400 bool
	for _, e := range p.events.Snapshot() {
		if e.Type == "end" && e.Status == http.StatusBadRequest {
			sawEnd400 = true
		}
	}
	if !sawEnd400 {
		t.Errorf("blocked request produced no 400 end event")
	}
}

func TestGuard_OffDisablesScanning(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "off")

	postOK(t, proxyURL+"/v1/chat/completions", guardRequestBody())

	got := bodies()
	if len(got) != 1 || !strings.Contains(got[0], guardFixtureKey) {
		t.Errorf("off: upstream should receive the unmodified body once (calls=%d)", len(got))
	}
	if details := guardEventDetails(p); len(details) != 0 {
		t.Errorf("off: guard events = %v, want none", details)
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "aws_access_key_id"}].Requests; n != 0 {
		t.Errorf("off: guard hit counter = %d, want 0", n)
	}
}

// Clean bodies take no guard action in any mode (no events, no rewrites).
func TestGuard_CleanBodyUnscathed(t *testing.T) {
	p, proxyURL, bodies := newGuardTestProxy(t, "block")

	postOK(t, proxyURL+"/v1/chat/completions", `{"model":"glm","messages":[{"role":"user","content":"explain the observer pattern"}]}`)

	got := bodies()
	if len(got) != 1 || !strings.Contains(got[0], "observer pattern") {
		t.Errorf("clean body should reach the upstream unmodified (calls=%d)", len(got))
	}
	if details := guardEventDetails(p); len(details) != 0 {
		t.Errorf("clean body: guard events = %v, want none", details)
	}
}
