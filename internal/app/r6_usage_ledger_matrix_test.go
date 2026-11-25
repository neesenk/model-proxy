package app

// r6_usage_ledger_matrix_test.go pins the two request-scoped usage ledgers to
// ONE merge rule per wire shape: the hot-path UsageScanner (counters → minute
// buckets / Accounts / Analytics / Agents) and requestlog.ExtractUsage
// (request-log index → Keys usage, session aggregates, and the boot-time
// ledger rebuild that overwrites historical minute buckets from the index).
// usage_ledger_test.go covers single frames; this file covers multi-frame
// sequences, where the merge rule — not the frame decode — decides the value.
// A stream whose frames disagree with each other must read the same on both
// sides, or the rebuild silently rewrites history the live counters reported.

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
)

// lineReadCloser hands a UsageScanner one line per Read call (newline
// included), so every usage frame lands on a Read boundary.
type lineReadCloser struct {
	r    *bufio.Reader
	rest []byte
}

func (l *lineReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(l.rest) == 0 {
		next, err := l.r.ReadBytes('\n')
		if len(next) == 0 {
			return 0, err
		}
		l.rest = next
	}
	n := copy(p, l.rest)
	l.rest = l.rest[n:]
	return n, nil
}

func (l *lineReadCloser) Close() error { return nil }

// matrixLedgers feeds one response body through both ledgers and returns
// their four-bucket readings.
func matrixLedgers(t *testing.T, body []byte) (counters.TokenUsage, requestlog.Usage) {
	t.Helper()
	tc := counters.NewTokenCounter()
	key := counters.TokenKey{Provider: "p", Model: "m"}
	s := counters.NewUsageScanner(&lineReadCloser{r: bufio.NewReader(bytes.NewReader(body))}, key, tc, nil)
	buf := make([]byte, 64*1024)
	for {
		_, err := s.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("scanner read: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("scanner close: %v", err)
	}
	return tc.Snapshot()[key], requestlog.ExtractUsage(string(body))
}

// assertLedger checks one sample: the two ledgers agree, and both equal the
// sample's absolute truth.
func assertLedger(t *testing.T, label string, body []byte, want requestlog.Usage) {
	t.Helper()
	sc, rl := matrixLedgers(t, body)
	got := requestlog.Usage{Input: sc.Input, Output: sc.Output, CacheRead: sc.CacheRead, CacheCreation: sc.CacheCreation}
	if got != rl {
		t.Errorf("%s: ledgers diverge — counters=%+v requestlog=%+v (truth %+v)", label, got, rl, want)
	}
	if got != want {
		t.Errorf("%s: counters = %+v, want %+v", label, got, want)
	}
	if rl != want {
		t.Errorf("%s: requestlog = %+v, want %+v", label, rl, want)
	}
}

// frame builds one SSE frame from data lines joined per the SSE spec.
func frame(data ...string) string {
	return "data: " + strings.Join(data, "\ndata: ") + "\n\n"
}

// TestUsageLedgerMatrix_MultiFrameSequences pins the per-shape merge rule on
// hand-written multi-frame streams. Each sample's expectation is the value
// the index ledger reports — the rule the boot-time rebuild assumes — and
// the hot-path counter must report the same.
func TestUsageLedgerMatrix_MultiFrameSequences(t *testing.T) {
	anthropicStream := func(start string, deltas ...string) string {
		var b strings.Builder
		b.WriteString(frame(`{"type":"message_start","message":{"usage":` + start + `}}`))
		for _, d := range deltas {
			b.WriteString(frame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + d + `}`))
		}
		b.WriteString(frame(`{"type":"message_stop"}`))
		return b.String()
	}
	cases := []struct {
		name string
		body string
		want requestlog.Usage
	}{
		// --- chat dialect: the input/cache split is ONE reading taken from the
		// frame with the largest prompt_tokens (ties: the later frame); output
		// is the frame maximum.
		{
			name: "chat cumulative repeat, constant share",
			body: frame(`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":100}}}`) +
				frame(`{"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":100}}}`) +
				frame(`[DONE]`),
			want: requestlog.Usage{Input: 900, Output: 20, CacheRead: 100},
		},
		{
			name: "chat share grows between frames",
			body: frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":100}}}`) +
				frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":200}}}`),
			want: requestlog.Usage{Input: 800, Output: 20, CacheRead: 200},
		},
		{
			name: "chat prompt flat, share shrinks — the later frame's split stands",
			body: frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":300}}}`) +
				frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":100}}}`),
			want: requestlog.Usage{Input: 900, Output: 20, CacheRead: 100},
		},
		{
			name: "chat per-chunk usage: early chunks omit the cache detail (step-plan)",
			body: frame(`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":42750,"completion_tokens":1}}`) +
				frame(`{"choices":[{"delta":{"content":"b"}}],"usage":{"prompt_tokens":42750,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":1536}}}`) +
				frame(`{"choices":[],"usage":{"prompt_tokens":42750,"completion_tokens":3725,"prompt_tokens_details":{"cached_tokens":1536}}}`) +
				frame(`[DONE]`),
			want: requestlog.Usage{Input: 41214, Output: 3725, CacheRead: 1536},
		},
		{
			name: "chat split comes from the largest-prompt frame, not the latest",
			body: frame(`{"usage":{"prompt_tokens":1500,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":100}}}`) +
				frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":900}}}`),
			want: requestlog.Usage{Input: 1400, Output: 20, CacheRead: 100},
		},
		{
			name: "chat output flat then grows",
			body: frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":10}}`) +
				frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":10}}`) +
				frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":20}}`),
			want: requestlog.Usage{Input: 1000, Output: 20},
		},
		{
			name: "chat output is the frame maximum",
			body: frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":20}}`) +
				frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":10}}`),
			want: requestlog.Usage{Input: 1000, Output: 20},
		},
		{
			name: "chat write share is part of the same split",
			body: frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":100,"cache_write_tokens":300}}}`) +
				frame(`{"usage":{"prompt_tokens":1000,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":100,"cache_write_tokens":50}}}`),
			want: requestlog.Usage{Input: 850, Output: 20, CacheRead: 100, CacheCreation: 50},
		},
		{
			// deepseek dialect: the hit share may arrive on its own frame
			// before the frame that carries prompt_tokens. A hit-only frame is
			// still chat dialect (prompt 0) and its split is superseded whole
			// by the later, larger frame.
			name: "chat cache-hit-only frame precedes the full frame (deepseek)",
			body: frame(`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_cache_hit_tokens":4242}}`) +
				frame(`{"choices":[],"usage":{"prompt_tokens":900,"completion_tokens":10,"prompt_cache_hit_tokens":4242}}`),
			want: requestlog.Usage{Input: 0, Output: 10, CacheRead: 4242},
		},
		// --- anthropic message stream: message_start figures merge by field
		// max; a field message_delta carries (explicit zero included) replaces
		// them; output is the frame maximum.
		{
			name: "anthropic native: message_delta carries only output",
			body: anthropicStream(`{"input_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":1}`,
				`{"output_tokens":42}`),
			want: requestlog.Usage{Input: 25, Output: 42, CacheRead: 40, CacheCreation: 10},
		},
		{
			name: "anthropic native stream cut before message_delta",
			body: anthropicStream(`{"input_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":1}`),
			want: requestlog.Usage{Input: 25, Output: 1, CacheRead: 40, CacheCreation: 10},
		},
		{
			name: "anthropic kimi-code: input moved into cache_read at the terminal event",
			body: anthropicStream(`{"input_tokens":222,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`,
				`{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":222,"output_tokens":82}`),
			want: requestlog.Usage{Output: 82, CacheRead: 222},
		},
		{
			name: "anthropic step-plan: placeholder split at message_start, real split at message_delta",
			body: anthropicStream(`{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":10533,"output_tokens":0}`,
				`{"input_tokens":5670,"cache_creation_input_tokens":0,"cache_read_input_tokens":4864,"output_tokens":672}`),
			want: requestlog.Usage{Input: 5670, Output: 672, CacheRead: 4864},
		},
		{
			name: "anthropic explicit zero at message_delta is final",
			body: anthropicStream(`{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":4925,"output_tokens":0}`,
				`{"input_tokens":4926,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":393}`),
			want: requestlog.Usage{Input: 4926, Output: 393},
		},
		{
			name: "anthropic deepseek: the same cumulative values in both frames count once",
			body: anthropicStream(`{"input_tokens":39,"cache_creation_input_tokens":0,"cache_read_input_tokens":256,"output_tokens":0}`,
				`{"input_tokens":39,"cache_creation_input_tokens":0,"cache_read_input_tokens":256,"output_tokens":72}`),
			want: requestlog.Usage{Input: 39, Output: 72, CacheRead: 256},
		},
		{
			name: "anthropic message_delta: input by presence, output by max",
			body: anthropicStream(`{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0}`,
				`{"input_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":30,"output_tokens":20}`,
				`{"input_tokens":80,"cache_creation_input_tokens":0,"cache_read_input_tokens":60,"output_tokens":10}`),
			want: requestlog.Usage{Input: 80, Output: 20, CacheRead: 60},
		},
		{
			name: "anthropic message_start fields merge by max",
			body: frame(`{"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":4,"output_tokens":1}}}`) +
				frame(`{"type":"message_start","message":{"usage":{"input_tokens":50,"cache_read_input_tokens":6,"output_tokens":1}}}`) +
				frame(`{"type":"message_stop"}`),
			want: requestlog.Usage{Input: 100, Output: 1, CacheRead: 6},
		},
		// --- responses: one cumulative snapshot per field max.
		{
			name: "responses terminal frame",
			body: frame(`{"type":"response.created","response":{"usage":null}}`) +
				frame(`{"type":"response.in_progress","response":{"usage":null}}`) +
				frame(`{"type":"response.completed","response":{"usage":{"input_tokens":200,"output_tokens":9,"input_tokens_details":{"cached_tokens":150,"cache_write_tokens":20}}}}`),
			want: requestlog.Usage{Input: 30, Output: 9, CacheRead: 150, CacheCreation: 20},
		},
		{
			name: "responses snapshots merge per field by max",
			body: frame(`{"type":"response.completed","response":{"usage":{"input_tokens":200,"output_tokens":9,"input_tokens_details":{"cached_tokens":150}}}}`) +
				frame(`{"type":"response.completed","response":{"usage":{"input_tokens":120,"output_tokens":11,"input_tokens_details":{"cached_tokens":100}}}}`),
			want: requestlog.Usage{Input: 50, Output: 11, CacheRead: 150},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertLedger(t, tc.name, []byte(tc.body), tc.want)
		})
	}
}

// TestUsageLedgerMatrix_RecordedWireStreams runs every recorded real stream
// through both ledgers: the four buckets must be equal field for field. An
// unreadable or empty fixture directory fails the test — an empty loop would
// be a false green.
func TestUsageLedgerMatrix_RecordedWireStreams(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "wire")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read wire fixtures: %v", err)
	}
	var files int
	var total requestlog.Usage
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sse") {
			continue
		}
		files++
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		sc, rl := matrixLedgers(t, raw)
		got := requestlog.Usage{Input: sc.Input, Output: sc.Output, CacheRead: sc.CacheRead, CacheCreation: sc.CacheCreation}
		if got != rl {
			t.Errorf("%s: ledgers diverge — counters=%+v requestlog=%+v", e.Name(), got, rl)
		}
		total.Input += rl.Input
		total.Output += rl.Output
		total.CacheRead += rl.CacheRead
		total.CacheCreation += rl.CacheCreation
	}
	if files == 0 {
		t.Fatalf("no recorded .sse streams under %s", dir)
	}
	// The loop must have read real usage: a harness that fed the ledgers
	// nothing would agree on zeros.
	if total.Input == 0 || total.Output == 0 || total.CacheRead == 0 {
		t.Fatalf("%d streams yielded no usage (input=%d output=%d cache_read=%d) — harness is not feeding the ledgers", files, total.Input, total.Output, total.CacheRead)
	}
}
