package protocol

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponsesState_RecordExpandAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses_state.json")
	s := newResponsesStateStore(path)
	history := []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "weather?"}},
	}}
	resp := []byte(`{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"weather","arguments":"{}"}]}`)
	if !s.recordJSON("sess", history, resp) {
		t.Fatal("recordJSON returned false")
	}
	s.close()

	s2 := newResponsesStateStore(path)
	defer s2.close()
	in := []byte(`{"model":"g","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`)
	got, expanded, hit, err := s2.expand(in, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if !hit || len(expanded) != 3 {
		t.Fatalf("hit=%v expanded=%v", hit, expanded)
	}
	var body map[string]any
	json.Unmarshal(got, &body)
	if _, ok := body["previous_response_id"]; ok {
		t.Fatalf("previous_response_id not stripped: %s", got)
	}
	if typ := strOf(asMap(expanded[1])["type"]); typ != "function_call" {
		t.Fatalf("restored item type = %q", typ)
	}
	if typ := strOf(asMap(expanded[2])["type"]); typ != "function_call_output" {
		t.Fatalf("continuation item type = %q", typ)
	}
}

func TestResponsesState_MissRepairsOrphanedOutput(t *testing.T) {
	s := newResponsesStateStore("")
	in := []byte(`{"model":"g","previous_response_id":"missing","input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"secret"}]},` +
		`{"type":"function_call_output","call_id":"c9","output":"result"}]}`)
	_, expanded, hit, err := s.expand(in, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if hit || len(expanded) != 1 {
		t.Fatalf("hit=%v expanded=%v", hit, expanded)
	}
	msg := asMap(expanded[0])
	if msg["type"] != "message" || msg["role"] != "user" {
		t.Fatalf("orphan was not repaired as user message: %v", msg)
	}
	if text := strOf(asMap(asSlice(msg["content"], 0))["text"]); !strings.Contains(text, "result") || !strings.Contains(text, "c9") {
		t.Fatalf("repair text = %q", text)
	}
}

// TestResponsesState_ExplicitHistoryNotRepaired: a stateless request WITHOUT
// previous_response_id asserts its own full history; orphan repair would
// silently change its semantics, so the items pass through untouched.
func TestResponsesState_ExplicitHistoryNotRepaired(t *testing.T) {
	s := newResponsesStateStore("")
	in := []byte(`{"model":"g","input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"kept"}]},` +
		`{"type":"function_call_output","call_id":"c9","output":"result"},` +
		`{"type":"function_call","call_id":"c10","name":"w","arguments":"{}"}]}`)
	out, expanded, hit, err := s.expand(in, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if hit || len(expanded) != 3 {
		t.Fatalf("hit=%v expanded=%v", hit, expanded)
	}
	if typ := strOpt(asMap(expanded[0])["type"]); typ != "reasoning" {
		t.Fatalf("reasoning dropped from explicit history: %v", expanded[0])
	}
	if typ := strOpt(asMap(expanded[1])["type"]); typ != "function_call_output" {
		t.Fatalf("orphan output rewritten in explicit history: %v", expanded[1])
	}
	if typ := strOpt(asMap(expanded[2])["type"]); typ != "function_call" {
		t.Fatalf("dangling call dropped from explicit history: %v", expanded[2])
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatal(err)
	}
	if n := len(anySlice(body["input"])); n != 3 {
		t.Fatalf("rewritten input length = %d: %s", n, out)
	}
}

func TestResponsesState_TTLExpiry(t *testing.T) {
	s := newResponsesStateStore("")
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	if !s.recordJSON("", []any{map[string]any{"type": "message"}}, []byte(`{"id":"r1","status":"completed","output":[]}`)) {
		t.Fatal("record failed")
	}
	now = now.Add(responsesStateTTL + time.Second)
	_, _, hit, err := s.expand([]byte(`{"model":"g","previous_response_id":"r1","input":"next"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("expired state was returned")
	}
}

func TestResponsesState_RecordsOnlyReplaySafeIncomplete(t *testing.T) {
	s := newResponsesStateStore("")
	history := []any{map[string]any{"type": "message", "role": "user"}}
	if s.recordJSON("sess", history, []byte(`{
		"id":"filtered","status":"incomplete",
		"incomplete_details":{"reason":"content_filter"},
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]
	}`)) {
		t.Fatal("content-filtered incomplete response was cached for replay")
	}
	if !s.recordJSON("sess", history, []byte(`{
		"id":"limited","status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]
	}`)) {
		t.Fatal("max_output_tokens incomplete response was not cached")
	}
	_, expanded, hit, err := s.expand([]byte(`{
		"model":"g","previous_response_id":"limited","input":"continue"
	}`), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if !hit || len(expanded) != 3 {
		t.Fatalf("replay-safe incomplete expansion hit=%v history=%#v", hit, expanded)
	}
}

// TestResponsesState_IDFallbackRequiresEmptySession: the unambiguous-id
// fallback exists only for clients without a stable session header; a caller
// that presents session B must not expand session A's history via A's
// response id.
func TestResponsesState_IDFallbackRequiresEmptySession(t *testing.T) {
	s := newResponsesStateStore("")
	history := []any{map[string]any{"type": "message", "role": "user"}}
	if !s.recordJSON("sessA", history, []byte(`{"id":"rA","status":"completed","output":[]}`)) {
		t.Fatal("record failed")
	}
	chain := []byte(`{"model":"g","previous_response_id":"rA","input":"next"}`)

	if _, _, hit, err := s.expand(chain, "sessB"); err != nil || hit {
		t.Fatalf("cross-session id lookup hit=%v err=%v, want miss", hit, err)
	}
	if _, _, hit, err := s.expand(chain, "sessA"); err != nil || !hit {
		t.Fatalf("same-session lookup hit=%v err=%v, want hit", hit, err)
	}
	if _, _, hit, err := s.expand(chain, ""); err != nil || !hit {
		t.Fatalf("sessionless unique-id lookup hit=%v err=%v, want hit", hit, err)
	}
}

// TestResponsesState_CapacityEviction: the entry-size cache (D1) must not
// change eviction semantics — prune (driven by every record/lookup) still
// drops the oldest entries once the count cap is exceeded, and every stored
// entry carries a precomputed serialized size.
func TestResponsesState_CapacityEviction(t *testing.T) {
	s := newResponsesStateStore("")
	for i := 0; i < responsesStateMax+8; i++ {
		id := fmt.Sprintf("r%d", i)
		body := fmt.Sprintf(`{"id":%q,"status":"completed","output":[]}`, id)
		if !s.recordJSON("", []any{map[string]any{"type": "message"}}, []byte(body)) {
			t.Fatalf("record %s failed", id)
		}
	}
	if len(s.entries) != responsesStateMax || len(s.order) != responsesStateMax {
		t.Fatalf("entries=%d order=%d, want %d", len(s.entries), len(s.order), responsesStateMax)
	}
	if _, ok := s.entries[responsesStateKey("", "r0")]; ok {
		t.Fatal("oldest entry was not evicted past the count cap")
	}
	newest := fmt.Sprintf("r%d", responsesStateMax+7)
	e, ok := s.entries[responsesStateKey("", newest)]
	if !ok {
		t.Fatalf("newest entry %s missing", newest)
	}
	if e.size <= 0 {
		t.Fatalf("cached size = %d, want > 0", e.size)
	}
	if got := responsesEntrySize(e); got != e.size {
		t.Fatalf("cached size = %d, re-marshal = %d (cache must match the persisted byte length)", e.size, got)
	}
}

// TestResponsesState_RestoreComputesCachedSize: entries loaded from disk have
// no size field (it is never persisted); load must compute it once so the
// total-byte accounting in pruneLocked still works after a restart.
func TestResponsesState_RestoreComputesCachedSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses_state.json")
	s := newResponsesStateStore(path)
	if !s.recordJSON("sess", []any{map[string]any{"type": "message"}}, []byte(`{"id":"r1","status":"completed","output":[]}`)) {
		t.Fatal("record failed")
	}
	s.close()

	s2 := newResponsesStateStore(path)
	defer s2.close()
	e, ok := s2.entries[responsesStateKey("sess", "r1")]
	if !ok {
		t.Fatal("entry not restored")
	}
	if e.size <= 0 {
		t.Fatalf("restored cached size = %d, want > 0", e.size)
	}
	if got := responsesEntrySize(e); got != e.size {
		t.Fatalf("restored cached size = %d, re-marshal = %d", e.size, got)
	}
}

// TestResponsesState_RecordsFoldedSSEFrames: a spec-folded multi-line data
// frame (payload split across data: lines) must parse once assembled —
// line-by-line parsing fails JSON on every fragment and silently loses both
// the response.output_item.done items and the terminal response.completed.
func TestResponsesState_RecordsFoldedSSEFrames(t *testing.T) {
	s := newResponsesStateStore("")
	history := []any{map[string]any{"type": "message", "role": "user"}}
	stream := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\n" +
		"data: \"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"lookup\",\"arguments\":\"{}\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\n" +
		"data: \"status\":\"completed\"}}\n\n"
	if !s.recordSSE("sess", history, []byte(stream)) {
		t.Fatal("folded SSE stream was not recorded")
	}
	_, expanded, hit, err := s.expand([]byte(`{"model":"g","previous_response_id":"r1","input":"next"}`), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("folded stream history not expanded")
	}
	// The completed frame carried no output array, so the recorded output must
	// come from the folded response.output_item.done item.
	found := false
	for _, item := range expanded {
		if strOpt(asMap(item)["call_id"]) == "c1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("folded output_item.done item lost from recorded history: %#v", expanded)
	}
}

// TestResponsesState_MergedFramesKeepPerDataEventAssociation covers the same
// missing-blank-line gateway defect in the state recorder. The function-call
// item must be handled as output_item.done before the following completed
// event records the response; assigning both payloads the final event loses
// the tool history used by previous_response_id expansion.
func TestResponsesState_MergedFramesKeepPerDataEventAssociation(t *testing.T) {
	s := newResponsesStateStore("")
	history := []any{map[string]any{"type": "message", "role": "user"}}
	stream := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"id\":\"fc1\",\"call_id\":\"c1\",\"name\":\"lookup\",\"arguments\":\"{}\"}}\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"
	if !s.recordSSE("sess", history, []byte(stream)) {
		t.Fatal("merged SSE stream was not recorded")
	}
	_, expanded, hit, err := s.expand([]byte(`{"model":"g","previous_response_id":"r1","input":"next"}`), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("merged stream history not expanded")
	}
	for _, item := range expanded {
		if strOpt(asMap(item)["call_id"]) == "c1" {
			return
		}
	}
	t.Fatalf("output_item.done lost from merged stream history: %#v", expanded)
}
