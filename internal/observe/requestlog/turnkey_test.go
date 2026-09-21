package requestlog

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestComputeTurnKeyOpenAIUserText(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"world"}]}`)
	key := computeTurnKey(body)
	if key == "" {
		t.Fatal("expected non-empty turn key for openai user text")
	}
	// Same newest text but a different real-user-text count (an earlier user
	// turn) must produce a different key.
	body2 := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"world"}]}`)
	key2 := computeTurnKey(body2)
	if key2 == "" {
		t.Fatal("expected non-empty turn key for two-user body")
	}
	if key == key2 {
		t.Fatalf("user-text count must change the key: %q == %q", key, key2)
	}
}

// TestComputeTurnKeyStableAcrossAgenticTurn is the Trace-segmentation
// regression: within one agentic turn each follow-up request appends assistant
// messages and tool_result user blocks, so a total-message-count hash changed
// per request and degenerated segmentation to one segment per request. The key
// must stay constant while the real-user-text count stays constant.
func TestComputeTurnKeyStableAcrossAgenticTurn(t *testing.T) {
	base := `{"messages":[
		{"role":"user","content":[{"type":"text","text":"refactor the parser"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{}}]}`
	mid := base + `,
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file contents"}]},
		{"role":"assistant","content":[{"type":"text","text":"done"}]}`
	full := mid + `,
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"more output"}]}
	]}`
	keys := map[string]bool{}
	for _, body := range []string{base + `]}`, mid + `]}`, full} {
		k := computeTurnKey([]byte(body))
		if k == "" {
			t.Fatal("expected non-empty turn key across agentic sub-requests")
		}
		keys[k] = true
	}
	if len(keys) != 1 {
		t.Fatalf("agentic turn produced %d distinct keys, want 1 stable key", len(keys))
	}
}

func TestComputeTurnKeyAnthropicSkipsToolResult(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"keep going"}]},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file contents"}]}
	]}`)
	key := computeTurnKey(body)
	// tool_result-only user messages do not count: one real user-text message.
	want := hashTurnKey(1, "keep going")
	if key != want {
		t.Fatalf("turn key = %q, want %q (last real user text)", key, want)
	}
}

func TestComputeTurnKeyArrayTextBlocks(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"part A "},{"type":"text","text":"part B"}]}]}`)
	key := computeTurnKey(body)
	want := hashTurnKey(1, "part A part B")
	if key != want {
		t.Fatalf("turn key = %q, want %q", key, want)
	}
}

func TestComputeTurnKeyResponsesInput(t *testing.T) {
	// responses API: input may be a string or a message array.
	str := []byte(`{"input":"string input"}`)
	if got := computeTurnKey(str); got != hashTurnKey(0, "string input") {
		t.Fatalf("string input turn key = %q, want %q", got, hashTurnKey(0, "string input"))
	}
	arr := []byte(`{"input":[{"role":"user","content":"array input"}]}`)
	if got := computeTurnKey(arr); got != hashTurnKey(1, "array input") {
		t.Fatalf("array input turn key = %q, want %q", got, hashTurnKey(1, "array input"))
	}
}

// TestComputeTurnKeySameTextNewTurn: the same literal text sent again as a new
// turn — after another real user message arrived — must produce a different
// key. (Appended assistant/tool traffic alone does NOT change the key; that is
// the agentic-turn stability case covered above.)
func TestComputeTurnKeySameTextNewTurn(t *testing.T) {
	body1 := []byte(`{"messages":[{"role":"user","content":"continue"}]}`)
	body2 := []byte(`{"messages":[{"role":"user","content":"ship it"},{"role":"assistant","content":"done"},{"role":"user","content":"continue"}]}`)
	key1 := computeTurnKey(body1)
	key2 := computeTurnKey(body2)
	if key1 == "" || key2 == "" {
		t.Fatal("expected non-empty keys")
	}
	if key1 == key2 {
		t.Fatalf("same text as a new user turn must differ: %q == %q", key1, key2)
	}
}

func TestComputeTurnKeyFallsBackWhenNoUserText(t *testing.T) {
	cases := []string{
		`{"messages":[{"role":"assistant","content":"no user"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`,
		`not valid json`,
		`{"model":"gpt-4"}`,
		"",
	}
	for _, c := range cases {
		if got := computeTurnKey([]byte(c)); got != "" {
			t.Errorf("computeTurnKey(%q) = %q, want empty", c, got)
		}
	}
}

func TestComputeTurnKeyOversizeBody(t *testing.T) {
	body := make([]byte, 1500001)
	for i := range body {
		body[i] = 'x'
	}
	if got := computeTurnKey(body); got != "" {
		t.Fatalf("oversize body must yield empty turn key, got %q", got)
	}
}

func TestBuildRecordPersistsTurnKey(t *testing.T) {
	logger := New(Options{Directory: t.TempDir(), MaxBodyBytes: 1 << 20})
	body := []byte(`{"messages":[{"role":"user","content":"turn one"}]}`)
	rec := logger.BuildRecord(Input{RequestID: "r1", RequestBody: body})
	want := hashTurnKey(1, "turn one")
	if rec.TurnKey != want {
		t.Fatalf("TurnKey = %q, want %q", rec.TurnKey, want)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte(`"turn_key":"`+want+`"`)) {
		t.Fatalf("marshaled record missing turn_key: %s", line)
	}
	// A body with no user text omits the field from the JSONL line.
	rec2 := logger.BuildRecord(Input{RequestID: "r2", RequestBody: []byte(`{"model":"x"}`)})
	if rec2.TurnKey != "" {
		t.Fatalf("expected empty TurnKey, got %q", rec2.TurnKey)
	}
	line2, _ := json.Marshal(rec2)
	if bytes.Contains(line2, []byte(`"turn_key"`)) {
		t.Fatalf("empty turn key must be omitted: %s", line2)
	}
}

func TestSummarizeCarriesTurnKey(t *testing.T) {
	rec := Record{Ts: "2026-09-16T00:00:00Z", RequestID: "r1", TurnKey: "abc123"}
	s := Summarize(rec)
	if s.TurnKey != "abc123" {
		t.Fatalf("Summarize TurnKey = %q, want abc123", s.TurnKey)
	}
}
