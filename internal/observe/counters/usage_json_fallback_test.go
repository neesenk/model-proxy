package counters

import (
	"io"
	"strings"
	"testing"
)

// scanBody feeds body through a UsageScanner in chunk-sized reads and returns
// the committed usage for key k.
func scanBody(k TokenKey, body string, chunk int) TokenUsage {
	tc := NewTokenCounter()
	s := NewUsageScanner(io.NopCloser(strings.NewReader(body)), k, tc, nil)
	buf := make([]byte, chunk)
	for {
		_, err := s.Read(buf)
		if err != nil {
			break
		}
	}
	_ = s.Close()
	return tc.Snapshot()[k]
}

func TestUsageScannerJSONFallback(t *testing.T) {
	key := TokenKey{Provider: "p", Model: "m"}

	t.Run("decisions/anthropic shape", func(t *testing.T) {
		body := `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.7}},"usage":{"input_tokens":431,"output_tokens":79}}`
		u := scanBody(key, body, 64)
		if u.Input != 431 || u.Output != 79 || u.Requests != 1 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("anthropic shape with cache buckets", func(t *testing.T) {
		body := `{"id":"msg_1","content":[],"usage":{"input_tokens":50,"output_tokens":9,"cache_creation_input_tokens":12,"cache_read_input_tokens":34}}`
		u := scanBody(key, body, 1024)
		if u.Input != 50 || u.Output != 9 || u.CacheCreation != 12 || u.CacheRead != 34 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("openai shape", func(t *testing.T) {
		body := `{"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`
		u := scanBody(key, body, 7)
		if u.Input != 100 || u.Output != 20 || u.CacheRead != 40 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("responses shape", func(t *testing.T) {
		body := `{"id":"resp_1","output":[],"usage":{"input_tokens":88,"output_tokens":11,"input_tokens_details":{"cached_tokens":22}}}`
		u := scanBody(key, body, 1024)
		if u.Input != 88 || u.Output != 11 || u.CacheRead != 22 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("leading whitespace still detects JSON", func(t *testing.T) {
		body := "  \n" + `{"usage":{"input_tokens":3,"output_tokens":1}}`
		u := scanBody(key, body, 1024)
		if u.Input != 3 || u.Output != 1 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("no usage object is a no-op", func(t *testing.T) {
		u := scanBody(key, `{"error":{"message":"bad request"}}`, 128)
		if u.Input != 0 || u.Output != 0 || u.Requests != 0 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("truncated body is a no-op", func(t *testing.T) {
		u := scanBody(key, `{"model":"m","usage":{"input_tok`, 128)
		if u.Input != 0 || u.Output != 0 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("SSE usage is not double counted", func(t *testing.T) {
		body := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":50}}}\n\n" +
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n" +
			"data: [DONE]\n\n"
		u := scanBody(key, body, 16)
		if u.Input != 50 || u.Output != 9 || u.Requests != 1 {
			t.Errorf("usage = %+v, want exactly one counting pass", u)
		}
	})
	t.Run("SSE stream without usage stays zero", func(t *testing.T) {
		body := "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\ndata: [DONE]\n\n"
		u := scanBody(key, body, 16)
		if u.Input != 0 || u.Output != 0 {
			t.Errorf("usage = %+v", u)
		}
	})
	t.Run("oversized body bounds retention and stays silent", func(t *testing.T) {
		body := `{"pad":"` + strings.Repeat("x", jsonBodyCap) + `","usage":{"input_tokens":5,"output_tokens":1}}`
		u := scanBody(key, body, 1<<20)
		if u.Input != 0 || u.Output != 0 {
			t.Errorf("usage = %+v, want no usage for an over-cap (unparseable) body", u)
		}
	})
}
