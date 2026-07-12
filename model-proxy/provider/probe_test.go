package provider

import (
	"net/http"
	"strings"
	"testing"
)

// probe_test.go covers the per-provider probe/filter behavior that lives in the
// provider implementations (probe.go baseProbe defaults + aqp/codex/volcengine
// overrides). These moved out of the main package's models_check_test.go when
// the probe path/body/header selection was extracted from main's
// `if prov.Provider == ...` branches into ProbeRequest/ExtraHeaders/FilterModelIDs.

// --- baseProbe defaults: OpenAI /chat/completions, no extra headers, passthrough filter ---

func TestBaseProbe_Defaults(t *testing.T) {
	var b baseProbe
	pr := b.ProbeRequest("glm-5.2")
	if pr.Method != http.MethodPost {
		t.Errorf("default Method=%q want POST", pr.Method)
	}
	if pr.Path != "/chat/completions" {
		t.Errorf("default Path=%q want /chat/completions", pr.Path)
	}
	body := string(pr.Body)
	if !strings.Contains(body, `"model":"glm-5.2"`) {
		t.Errorf("default body=%q want model field", body)
	}
	if !strings.Contains(body, `"messages"`) {
		t.Errorf("default body=%q want messages field (OpenAI chat shape)", body)
	}
	if strings.Contains(body, `"input"`) {
		t.Errorf("default body=%q must NOT use `input` (that's codex /responses)", body)
	}

	// ExtraHeaders is a no-op by default - a request's headers are untouched.
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	req.Header.Set("X-Marker", "keep")
	b.ExtraHeaders(req, "/chat/completions")
	if req.Header.Get("anthropic-version") != "" {
		t.Errorf("default ExtraHeaders set anthropic-version; want no-op")
	}
	if req.Header.Get("X-Marker") != "keep" {
		t.Errorf("default ExtraHeaders clobbered an existing header")
	}

	// FilterModelIDs passes through unchanged.
	ids := []string{"a", "b", "c"}
	kept, dropped := b.FilterModelIDs(ids)
	if len(kept) != 3 || len(dropped) != 0 {
		t.Errorf("default FilterModelIDs kept=%v dropped=%v want passthrough", kept, dropped)
	}
}

// --- aqp: anthropic /v1/messages probe + anthropic-version/compass-id headers ---

func newAqpForTest() *AqpProvider { return &AqpProvider{cfg: &Config{}} }

func TestAqpProbeRequest(t *testing.T) {
	pr := newAqpForTest().ProbeRequest("glm-5.2")
	if pr.Method != http.MethodPost {
		t.Errorf("aqp Method=%q want POST", pr.Method)
	}
	if pr.Path != "/v1/messages" {
		t.Errorf("aqp Path=%q want /v1/messages (anthropic; base no /v1, SDK appends)", pr.Path)
	}
	body := string(pr.Body)
	if !strings.Contains(body, `"model":"glm-5.2"`) {
		t.Errorf("aqp body=%q want model field", body)
	}
	if !strings.Contains(body, `"messages"`) {
		t.Errorf("aqp body=%q want messages field (anthropic shape)", body)
	}
	// anthropic-version is a HEADER (set by ExtraHeaders), NOT in the body.
	if strings.Contains(body, "anthropic-version") {
		t.Errorf("aqp body=%q must NOT contain anthropic-version (it's a header)", body)
	}
}

func TestAqpExtraHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	newAqpForTest().ExtraHeaders(req, "/v1/messages")
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("aqp anthropic-version=%q want 2023-06-01", got)
	}
	cid := req.Header.Get("x-compass-request-id")
	if cid == "" {
		t.Errorf("aqp x-compass-request-id not set")
	}
	// It's a UUID v4: 8-4-4-4-12 hex, version nibble 4.
	if !strings.Contains(cid, "-") || len(cid) != 36 {
		t.Errorf("aqp compass id=%q want UUID v4 (36 chars)", cid)
	}
	// Two calls produce DIFFERENT ids (per-request UUID, not a constant).
	req2, _ := http.NewRequest(http.MethodPost, "http://x", nil)
	newAqpForTest().ExtraHeaders(req2, "/v1/messages")
	if req2.Header.Get("x-compass-request-id") == cid {
		t.Errorf("aqp compass id reused across calls; want a fresh UUID each time")
	}
}

// --- codex: /responses probe, input list (not messages), stream:true, no max_tokens ---

func newCodexForTest() *CodexProvider { return &CodexProvider{cfg: &Config{}} }

func TestCodexProbeRequest(t *testing.T) {
	pr := newCodexForTest().ProbeRequest("gpt-5.5")
	if pr.Method != http.MethodPost {
		t.Errorf("codex Method=%q want POST", pr.Method)
	}
	if pr.Path != "/responses" {
		t.Errorf("codex Path=%q want /responses (Responses API, not /chat/completions)", pr.Path)
	}
	body := string(pr.Body)
	if !strings.Contains(body, `"stream":true`) {
		t.Errorf("codex body=%q want stream:true (backend requires it)", body)
	}
	if !strings.Contains(body, `"input":`) {
		t.Errorf("codex body=%q want `input` field (Responses API)", body)
	}
	if strings.Contains(body, `"messages"`) {
		t.Errorf("codex body=%q must NOT use `messages` (backend rejects: Unsupported parameter: messages)", body)
	}
	if strings.Contains(body, "max_tokens") {
		t.Errorf("codex body=%q must NOT contain max_tokens (backend rejects)", body)
	}
}

// --- volcengine: FilterModelIDs drops *-latest / doubao-seed-1-* / lite / mini ---

func newVolcengineForTest() *VolcengineProvider {
	return &VolcengineProvider{cfg: &Config{}}
}

func TestVolcengineFilterModelIDs(t *testing.T) {
	cases := []struct {
		id       string
		filtered bool
	}{
		// *-latest rolling aliases
		{"ark-code-latest", true},
		{"deepseek-latest", true},
		{"minimax-latest", true},
		{"kimi-latest", true},
		{"glm-latest", true},
		// doubao-seed-1-* (standard-Ark date-version ids the plan endpoint rejects)
		{"doubao-seed-1-8-251228", true},
		{"doubao-seed-1-6-251015", true},
		// doubao-seed-*-lite (suffix) + *-lite-* (mid)
		{"doubao-seed-2.0-lite", true},
		{"doubao-seed-2-0-lite-260428", true},
		// doubao-seed-*-mini (suffix)
		{"doubao-seed-2.0-mini", true},

		// kept: concrete family aliases callable on the plan endpoint
		{"doubao-seed-2.0-code", false},
		{"doubao-seed-2.0-pro", false},
		{"doubao-seed-2-0-code", false}, // hand-added short-dash form; no rule matches
		{"glm-5.2", false},
		{"deepseek-v4-pro", false},
		{"deepseek-v4-flash", false},
		{"kimi-k2.7-code", false},
		{"kimi-k2.6", false},
		{"minimax-m2.7", false},
		{"minimax-m3", false},
		// kept: the doubao-seed- prefix must not catch seedance / seedream
		{"doubao-embedding-vision", false},
		{"doubao-seedance-2.0", false},
		{"doubao-seedance-2.0-fast", false},
		{"doubao-seedance-1.5-pro", false},
		{"doubao-seedream-5.0-lite", false}, // ends in -lite but is seedream, not seed-
	}
	ids := make([]string, 0, len(cases))
	wantFiltered := map[string]bool{}
	for _, c := range cases {
		ids = append(ids, c.id)
		wantFiltered[c.id] = c.filtered
	}
	kept, dropped := newVolcengineForTest().FilterModelIDs(ids)
	// Every id lands in exactly one bucket, matching wantFiltered.
	seen := map[string]bool{}
	for _, id := range kept {
		if seen[id] {
			t.Errorf("kept duplicate %q", id)
		}
		seen[id] = true
		if wantFiltered[id] {
			t.Errorf("FilterModelIDs kept %q; want dropped", id)
		}
	}
	for _, id := range dropped {
		if seen[id] {
			t.Errorf("dropped duplicate %q", id)
		}
		seen[id] = true
		if !wantFiltered[id] {
			t.Errorf("FilterModelIDs dropped %q; want kept", id)
		}
	}
	if len(seen) != len(cases) {
		t.Errorf("FilterModelIDs lost ids: seen=%d want=%d", len(seen), len(cases))
	}
}

// --- zhipu/deepseek: default probe + passthrough filter (embed baseProbe, no override) ---

func TestZhipuDefaults(t *testing.T) {
	p := &ZhipuProvider{cfg: &Config{}}
	pr := p.ProbeRequest("glm-5.2")
	if pr.Path != "/chat/completions" {
		t.Errorf("zhipu Path=%q want /chat/completions (default)", pr.Path)
	}
	kept, dropped := p.FilterModelIDs([]string{"glm-latest", "glm-5.2"})
	if len(kept) != 2 || len(dropped) != 0 {
		t.Errorf("zhipu FilterModelIDs kept=%v dropped=%v want passthrough (no policy rules)", kept, dropped)
	}
}

func TestDeepseekDefaults(t *testing.T) {
	p := &DeepSeekProvider{cfg: &Config{}}
	pr := p.ProbeRequest("deepseek-v4-pro")
	if pr.Path != "/chat/completions" {
		t.Errorf("deepseek Path=%q want /chat/completions (default)", pr.Path)
	}
}
