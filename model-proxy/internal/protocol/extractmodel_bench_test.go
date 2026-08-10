package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// --- fixtures of varying size ---

// 2KB: a minimal request (no tools, short conversation)
var body2K = []byte(`{"model":"claude-sonnet-4-20250514","max_tokens":8192,"system":"You are a helpful assistant.","messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":"Hi there!"},{"role":"user","content":"What is 2+2?"}]}`)

// 50KB: typical Claude Code request (tools + 5-turn history)
var body50K = func() []byte {
	msgs := `[{"role":"user","content":"Fix the bug in main.go"},{"role":"assistant","content":[{"type":"text","text":"Let me read the file first."},{"type":"tool_use","id":"toolu_01","name":"read","input":{"path":"main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"func main() {\n  fmt.Println(\"hello\")\n}"}]},{"role":"assistant","content":"I see the issue. Let me fix it."},{"role":"user","content":"Did you fix it yet?"}]`
	tools := `[{"name":"bash","description":"Run a command","input_schema":{"type":"object","properties":{"command":{"type":"string","description":"The command"}},"required":["command"]}},{"name":"edit","description":"Edit a file","input_schema":{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}},{"name":"read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]`
	b := `{"model":"claude-sonnet-4-20250514","max_tokens":8192,"system":"You are a helpful coding assistant. Always use tools to explore the codebase before making changes. Be thorough and precise. Follow the user's instructions carefully. Write clean, well-tested code. Explain your reasoning.",`
	b += `"tools":` + tools + `,`
	b += `"messages":` + msgs
	// Pad with history to reach ~50KB
	b += strings.Repeat(`,{"role":"user","content":"Tell me more about this approach and why it works."},{"role":"assistant","content":"Sure, the key insight is that we can leverage the existing infrastructure while adding the new functionality on top. This way we minimize the risk of breaking existing behavior. The implementation follows the pattern already established in the codebase, using the same interfaces and conventions."}`, 15)
	b += `}`
	return []byte(b)
}()

// 200KB: heavy session (many tools + long history)
var body200K = func() []byte {
	b := `{"model":"claude-sonnet-4-20250514","max_tokens":8192,"system":"You are a helpful assistant.",`
	b += `"tools":[`
	for i := 0; i < 20; i++ {
		if i > 0 {
			b += ","
		}
		b += `{"name":"tool_` + string(rune('A'+i)) + `","description":"Tool ` + string(rune('A'+i)) + ` does something useful with the given parameters and returns a result","input_schema":{"type":"object","properties":{"param1":{"type":"string","description":"The first parameter"},"param2":{"type":"integer","description":"The second parameter"},"param3":{"type":"boolean","description":"The third parameter"}},"required":["param1"]}}`
	}
	b += `],"messages":[`
	for i := 0; i < 50; i++ {
		if i > 0 {
			b += ","
		}
		b += `{"role":"user","content":"This is message number ` + string(rune('0'+i%10)) + `. ` + strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20) + `"}`
		b += `,{"role":"assistant","content":"` + strings.Repeat("I understand your request and will help you with that. ", 20) + `"}`
	}
	b += `]}`
	return []byte(b)
}()

// --- implementations ---

func extractModelUnmarshal(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &v)
	return v.Model
}

func extractModelDecoder(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	// Consume opening {. model is always the first top-level key in every
	// known LLM client (Claude Code, codex, openai SDK, ...).
	dec.Token() // {
	firstTok, _ := dec.Token()
	firstKey, _ := firstTok.(string)
	if firstKey == "model" {
		valTok, _ := dec.Token()
		val, _ := valTok.(string)
		return val
	}
	// Non-standard key order: fall back to full unmarshal (rare, safe).
	var v struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &v)
	return v.Model
}

func extractModelByteSearch(body []byte) string {
	idx := bytes.Index(body, []byte(`"model"`))
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(`"model"`):]
	c := bytes.IndexByte(rest, ':')
	if c < 0 {
		return ""
	}
	rest = bytes.TrimLeft(rest[c+1:], " \t\n\r")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	end := bytes.IndexByte(rest[1:], '"')
	if end < 0 {
		return ""
	}
	return string(rest[1 : 1+end])
}

// --- benchmarks ---

func benchExtract(b *testing.B, body []byte, fn func([]byte) string) {
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		m := fn(body)
		if m != "claude-sonnet-4-20250514" {
			b.Fatalf("model=%q", m)
		}
	}
}

func BenchmarkExtractModel_Unmarshal_2K(b *testing.B) { benchExtract(b, body2K, extractModelUnmarshal) }
func BenchmarkExtractModel_Unmarshal_50K(b *testing.B) {
	benchExtract(b, body50K, extractModelUnmarshal)
}
func BenchmarkExtractModel_Unmarshal_200K(b *testing.B) {
	benchExtract(b, body200K, extractModelUnmarshal)
}

func BenchmarkExtractModel_Decoder_2K(b *testing.B)   { benchExtract(b, body2K, extractModelDecoder) }
func BenchmarkExtractModel_Decoder_50K(b *testing.B)  { benchExtract(b, body50K, extractModelDecoder) }
func BenchmarkExtractModel_Decoder_200K(b *testing.B) { benchExtract(b, body200K, extractModelDecoder) }

func BenchmarkExtractModel_ByteSearch_2K(b *testing.B) {
	benchExtract(b, body2K, extractModelByteSearch)
}
func BenchmarkExtractModel_ByteSearch_50K(b *testing.B) {
	benchExtract(b, body50K, extractModelByteSearch)
}
func BenchmarkExtractModel_ByteSearch_200K(b *testing.B) {
	benchExtract(b, body200K, extractModelByteSearch)
}
