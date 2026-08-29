package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/appapi"
	"model-proxy/internal/pricing"
)

// sessionsReadAPI overrides just the log directory; everything else stays the
// zero stub.
type sessionsReadAPI struct {
	testReadAPI
	dir string
}

func (a sessionsReadAPI) RequestLogDirectory() string { return a.dir }
func (a sessionsReadAPI) Pricing() appapi.PricingSnapshot {
	return appapi.PricingSnapshot{
		Overrides: map[string]pricing.Override{
			// $2/M input, $6/M output — easy mental math.
			"glm-4.7": {Input: 2, Output: 6},
		},
		Catalog: &pricing.Catalog{},
	}
}

// TestHandleSessions serves per-session aggregates off a real (temp) request
// log and prices them through the same Resolve/ComputeCost path as analytics.
func TestHandleSessions(t *testing.T) {
	dir := t.TempDir()
	record := map[string]any{
		"ts":         time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"request_id": "1", "session_id": "s1", "exposed": "glm",
		"called_model": "glm", "upstream_model": "glm-4.7", "provider": "zhipu",
		"status": 200, "latency_ms": 10,
		"response_body": `{"usage":{"input_tokens":1000000,"output_tokens":500000}}`,
	}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "requests-20260824-100000.log"), append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{Reads: sessionsReadAPI{dir: dir}, Commands: testCommandAPI{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	serveWebRequest(server, rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var payload struct {
		Enabled  bool `json:"enabled"`
		Sessions []struct {
			SessionID string  `json:"session_id"`
			Requests  int     `json:"requests"`
			CostUSD   float64 `json:"cost_usd"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Enabled || len(payload.Sessions) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	s := payload.Sessions[0]
	if s.SessionID != "s1" || s.Requests != 1 {
		t.Fatalf("session = %+v", s)
	}
	// 1M input × $2/M + 0.5M output × $6/M = $5.
	if s.CostUSD != 5 {
		t.Fatalf("cost = %v, want 5", s.CostUSD)
	}

	// Disabled request log → enabled:false, not an error.
	rec2 := httptest.NewRecorder()
	server2, err := New(Options{Reads: testReadAPI{}, Commands: testCommandAPI{}})
	if err != nil {
		t.Fatal(err)
	}
	serveWebRequest(server2, rec2, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec2.Code != http.StatusOK || !bytes.Contains(rec2.Body.Bytes(), []byte(`"enabled":false`)) {
		t.Fatalf("disabled status = %d body = %s", rec2.Code, rec2.Body)
	}
	// Disabled must serialize a NON-NULL empty array — `null` would break the
	// UI's iteration contract the same way as on /api/requests.
	if body := rec2.Body.String(); !strings.Contains(body, `"sessions":[]`) {
		t.Fatalf("disabled sessions body must carry sessions:[] — got %s", body)
	}
}
