package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestForward_EarlyEventsHaveRequestID (bug 7): live events published on the
// EARLY-return paths (missing model 400, unrouted model 502, unknown path 502)
// used to carry an empty request_id — requestID was generated deep in forward
// (at the start-event), after these returns. The contract (fusion-shadow-cache
// "forward 产生 start/end，含稳定 request_id；cache hit、400/502 终局也必须产生
// end") requires every event to carry a stable id so start↔end pairing works.
func TestForward_EarlyEventsHaveRequestID(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
`))
	p := newTestProxy(t, cfg)

	eventCursor := 0
	endEventIDs := func() []string {
		recent := p.events.Snapshot()
		var ids []string
		for _, e := range recent[eventCursor:] {
			if e.Type == "end" {
				ids = append(ids, e.RequestID)
			}
		}
		eventCursor = len(recent)
		return ids
	}
	do := func(body, path string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		p.Handler(rec, req)
		return rec.Code
	}

	// Missing model field → 400 terminal end event with a non-empty request_id.
	if code := do(`{"stream":false}`, "/v1/chat/completions"); code != http.StatusBadRequest {
		t.Errorf("missing-model: status=%d, want 400", code)
	}
	ids := endEventIDs()
	if len(ids) == 0 {
		t.Fatalf("missing-model: expected an end event, got none")
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("missing-model end event has empty request_id")
		}
	}

	// Model not in routes → 502 terminal end event with a non-empty request_id.
	if code := do(`{"model":"no-such-route","stream":false}`, "/v1/chat/completions"); code != http.StatusBadGateway {
		t.Errorf("model-not-found: status=%d, want 502", code)
	}
	ids = endEventIDs()
	if len(ids) == 0 {
		t.Fatalf("model-not-found: expected an end event, got none")
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("model-not-found end event has empty request_id")
		}
	}

	// Unknown path (proto=="") → handler emits a terminal end event (502 终局也
	// 必须产生 end) with a non-empty request_id. Previously: no event at all.
	if code := do(`{}`, "/v1/no-such-path"); code != http.StatusBadGateway {
		t.Errorf("unknown-path: status=%d, want 502", code)
	}
	ids = endEventIDs()
	if len(ids) == 0 {
		t.Fatalf("unknown-path: expected a terminal end event, got none")
	}
	for _, id := range ids {
		if id == "" {
			t.Errorf("unknown-path end event has empty request_id")
		}
	}
}
