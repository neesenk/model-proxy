package requestlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// aggregatedChat is what aggregatorDouble folds every SSE body into: a chat
// completion carrying the same usage (3 in / 1 out) as testStream.
const aggregatedChat = `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`

const testStream = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n" +
	"data: [DONE]\n\n"

// aggregatorDouble stands in for the composition root's protocol aggregator
// (requestlog does not import the protocol package): it folds an SSE body to
// aggregatedChat, fails on bodies carrying FAIL, and returns a fold whose
// usage diverges from the stream's for bodies carrying DIVERGE.
func aggregatorDouble(calls *atomic.Int32) func(string, []byte) ([]byte, error) {
	return func(protocol string, body []byte) ([]byte, error) {
		calls.Add(1)
		switch {
		case protocol != "openai":
			return nil, errors.New("unsupported protocol")
		case strings.Contains(string(body), "FAIL"):
			return nil, errors.New("stream ended without finish_reason")
		case strings.Contains(string(body), "DIVERGE"):
			return []byte(`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":9}}`), nil
		}
		return []byte(aggregatedChat), nil
	}
}

func readLoggedRecords(t *testing.T, dir string) map[string]Record {
	t.Helper()
	got := map[string]Record{}
	for _, name := range requestLogFileNames(t, dir) {
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for scanner.Scan() {
			if len(strings.TrimSpace(scanner.Text())) == 0 {
				continue // the sink separates records with a blank line
			}
			var record Record
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				t.Fatalf("decode %s: %v", name, err)
			}
			got[record.RequestID] = record
		}
		_ = file.Close()
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

// TestLoggerStoresCompletedStreamsAggregated: with a stream aggregator wired,
// a completed 2xx LLM stream is stored as its aggregated JSON response and
// flagged response_aggregated — a long SSE answer is ~100 bytes of chunk
// envelope per output token, the aggregate a few percent of that. Everything
// whose raw bytes are the evidence keeps them: a truncated capture, a non-2xx
// answer, an MCP exchange, a non-SSE body (the aggregator is not even
// consulted for those), a stream the aggregator rejects (no terminal, an
// in-stream error) and a fold that would change the usage the index and
// session costs read from the body.
func TestLoggerStoresCompletedStreamsAggregated(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	logger := New(Options{Directory: dir, MaxBodyBytes: 1 << 20, AggregateStream: aggregatorDouble(&calls)})
	go logger.Run()
	complete := func(id string, mutate func(*Input), body string, truncated bool) {
		input := Input{RequestID: id, Protocol: "openai", Status: 200}
		if mutate != nil {
			mutate(&input)
		}
		Complete(logger, input, []byte(body), int64(len(body)), truncated)
	}
	complete("aggregated", nil, testStream, false)
	complete("truncated", nil, testStream, true)
	complete("error-status", func(in *Input) { in.Status = 502 }, testStream, false)
	complete("mcp", func(in *Input) { in.Kind = "mcp" }, testStream, false)
	complete("json-body", nil, `{"usage":{"prompt_tokens":3,"completion_tokens":1}}`, false)
	complete("fold-fails", nil, "data: FAIL\n\n"+testStream, false)
	complete("usage-diverges", nil, "data: DIVERGE\n\n"+testStream, false)
	logger.Shutdown()

	records := readLoggedRecords(t, dir)
	if got := records["aggregated"]; !got.ResponseAggregated || got.ResponseBody != aggregatedChat || got.ResponseSize != int64(len(testStream)) {
		t.Errorf("completed stream = aggregated %v, body %q, size %d; want the aggregate (wire size %d kept)",
			got.ResponseAggregated, got.ResponseBody, got.ResponseSize, len(testStream))
	}
	for id, raw := range map[string]string{
		"truncated":      testStream + truncationMarker,
		"error-status":   testStream,
		"mcp":            testStream,
		"json-body":      `{"usage":{"prompt_tokens":3,"completion_tokens":1}}`,
		"fold-fails":     "data: FAIL\n\n" + testStream,
		"usage-diverges": "data: DIVERGE\n\n" + testStream,
	} {
		got, ok := records[id]
		if !ok {
			t.Errorf("record %s missing", id)
			continue
		}
		if got.ResponseAggregated || got.ResponseBody != raw {
			t.Errorf("record %s = aggregated %v, body %q; want the raw body kept", id, got.ResponseAggregated, got.ResponseBody)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("aggregator consulted %d times, want 3 (only completed 2xx LLM SSE bodies)", got)
	}
}
