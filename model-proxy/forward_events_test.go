package main

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
	p := NewProxy(cfg)
	defer p.Close()

	endEventIDs := func() []string {
		p.events.mu.Lock()
		defer p.events.mu.Unlock()
		var ids []string
		for _, e := range p.events.recent {
			if e.Type == "end" {
				ids = append(ids, e.RequestID)
			}
		}
		p.events.recent = p.events.recent[:0]
		return ids
	}
	do := func(body, path string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		p.handler(rec, req)
	}

	// Missing model field → 400 terminal end event with a non-empty request_id.
	do(`{"stream":false}`, "/v1/chat/completions")
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
	do(`{"model":"no-such-route","stream":false}`, "/v1/chat/completions")
	for _, id := range endEventIDs() {
		if id == "" {
			t.Errorf("model-not-found end event has empty request_id")
		}
	}

	// Unknown path (proto=="") → handler emits a terminal end event (502 终局也
	// 必须产生 end) with a non-empty request_id. Previously: no event at all.
	do(`{}`, "/v1/no-such-path")
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
