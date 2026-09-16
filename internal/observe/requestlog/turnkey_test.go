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
	// Same text but a different total message count must produce a different key.
	body2 := []byte(`{"messages":[{"role":"user","content":"world"}]}`)
	key2 := computeTurnKey(body2)
	if key2 == "" {
		t.Fatal("expected non-empty turn key for single-message body")
	}
	if key == key2 {
		t.Fatalf("message count must change the key: %q == %q", key, key2)
	}
}

func TestComputeTurnKeyAnthropicSkipsToolResult(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"keep going"}]},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file contents"}]}
	]}`)
	key := computeTurnKey(body)
	want := hashTurnKey(3, "keep going")
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

func TestComputeTurnKeySameTextDifferentCount(t *testing.T) {
	body1 := []byte(`{"messages":[{"role":"user","content":"continue"}]}`)
	body2 := []byte(`{"messages":[{"role":"assistant","content":"ok"},{"role":"user","content":"continue"}]}`)
	key1 := computeTurnKey(body1)
	key2 := computeTurnKey(body2)
	if key1 == "" || key2 == "" {
		t.Fatal("expected non-empty keys")
	}
	if key1 == key2 {
		t.Fatalf("same text in different message counts must differ: %q == %q", key1, key2)
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
