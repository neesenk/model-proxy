package protocol

import (
	"bytes"
	"strings"
	"testing"
)

// FuzzParseWireSSE pins the wire parser's framing contract on arbitrary
// upstream bytes (malformed SSE is routine in the wild — missing blank
// lines, CRLF, comments, truncated frames). Invariants:
//   - an error (oversized payload) yields no events; otherwise no panic;
//   - parsing is deterministic;
//   - every emitted event is verbatim input material: event names are
//     copied from "event:" lines, and any single-line data payload (the
//     common case, and the only shape recoverWireSSEFrames can re-split
//     folded payloads into) must appear in the input byte-for-byte.
//     Multi-line payloads are folded joins and excluded from the substring
//     assertion.
func FuzzParseWireSSE(f *testing.F) {
	seeds := []string{
		"data: {\"id\":1}\n\ndata: {\"id\":2}\n\ndata: [DONE]\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"data:\ndata: x\n\n",
		"data: x\r\n\r\n",
		": keep-alive comment\n\n",
		"event: only\n\n",
		"data",
		"data:unspaced\n\n",
		"data: {\"a\":1}\ndata: {\"b\":2}\n\n",
		"data: [DONE]",
		"",
		"\n\n\n",
		"data: 中文 payload\n\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		events, err := parseWireSSE(raw)
		if err != nil {
			if len(events) != 0 {
				t.Fatalf("parseWireSSE errored yet returned %d events", len(events))
			}
			return
		}
		again, err2 := parseWireSSE(raw)
		if err2 != nil || len(again) != len(events) {
			t.Fatalf("parseWireSSE not deterministic: %d vs %d events (err2=%v)", len(events), len(again), err2)
		}
		for i := range events {
			if again[i].event != events[i].event || again[i].data != events[i].data {
				t.Fatalf("parseWireSSE event %d differs between runs", i)
			}
			if events[i].event != "" && !bytes.Contains(raw, []byte(events[i].event)) {
				t.Fatalf("event name %q is not verbatim input material", events[i].event)
			}
			if d := events[i].data; d != "" && !strings.Contains(d, "\n") && !bytes.Contains(raw, []byte(d)) {
				t.Fatalf("single-line data %q is not verbatim input material", d)
			}
		}
	})
}
