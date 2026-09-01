package cli

// wire_record_cmd.go implements `model-proxy wire record <provider>`: capture
// RAW upstream SSE streams (responses/chat/anthropic) into testdata/wire/ as
// <proto>_<provider>.sse, for the golden-replay tests in internal/protocol.
// One minimal stream=true request per endpoint, built with the provider's own
// call rules (RewriteRequest → AuthHeaders → prov.Headers → ExtraHeaders, same
// order as probeModelCallable). Credentials come from `login` — never from
// flags, and the recorded files contain no auth material (response bytes only;
// still review prompts before committing recordings).
//
//	wire record <provider> [--model M] [--prompt P] [--out DIR]
//
// A non-2xx endpoint is recorded as <proto>_<provider>.err (status + body
// excerpt) and never overwrites an existing good .sse file.

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"model-proxy/internal/provider"
)

// wireRecordTimeout caps one recording request.
const wireRecordTimeout = 30 * time.Second

func CmdWire(args []string, cfg *configdomain.Config) {
	if len(args) == 0 || args[0] != "record" {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy wire record <provider> [--model M] [--prompt P] [--out DIR]\n", provider.Red("✗"))
		os.Exit(1)
	}
	CmdWireRecord(args[1:], cfg)
}

func CmdWireRecord(args []string, cfg *configdomain.Config) {
	fs := flag.NewFlagSet("wire record", flag.ExitOnError)
	model := fs.String("model", "", "model id for the probe request (default: provider's first model)")
	prompt := fs.String("prompt", "Say hi in one short sentence.", "prompt text")
	outDir := fs.String("out", filepath.Join("testdata", "wire"), "output directory for .sse recordings")
	// Go's flag package stops at the first non-flag arg, so split interspersed
	// flags out manually — `wire record <provider> --out DIR` must work. The
	// global --config/--log-file flags are scanned by configPath, not our
	// FlagSet — strip them (with their values) before parsing, or ExitOnError
	// kills the process on a perfectly valid command line.
	flagArgs, pos := SplitWireRecordArgs(args)
	flagArgs = StripGlobalFlags(flagArgs)
	fs.Parse(flagArgs)
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy wire record <provider> [--model M] [--prompt P] [--out DIR]\n", provider.Red("✗"))
		os.Exit(1)
	}
	provName := pos[0]

	if err := RunWireRecord(provName, *model, *prompt, *outDir, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", provider.Red("✗"), err)
		os.Exit(1)
	}
}

// runWireRecord is the testable core of `wire record`: record all endpoints
// for one provider into outDir. Returns an error for unknown provider, build
// failure, or when any endpoint failed (failures are printed as they happen).
func RunWireRecord(provName, model, prompt, outDir string, cfg *configdomain.Config) error {
	provCfg, ok := cfg.Providers[provName]
	if !ok {
		return fmt.Errorf("unknown provider %q", provName)
	}
	impl, err := climodels.ProviderImplFor(cfg, provName)
	if err != nil {
		return err
	}
	m := model
	if m == "" {
		m = wireProbeModelLocal(cfg, nil, provName)
	}
	if m == "" {
		return fmt.Errorf("no model for provider %q (pass --model)", provName)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	qm, qp := JSONQuote(m), JSONQuote(prompt)
	// One endpoint = one protocol × one scenario. Scenarios: text (bare stem),
	// tool (tool call), thinking (reasoning enabled) — the last two
	// exercise the hard conversion paths (tool_use/tool_calls/function_call
	// streaming, thinking/reasoning blocks) that a bare text stream never hits.
	// tool_choice stays "auto" (with an imperative prompt): thinking-mode
	// providers (deepseek, qwen-plan) 400 on forced tool_choice — and when a
	// model still doesn't call, the golden survival assertion just skips (it
	// gates on input markers). Unsupported scenarios land in .err.
	type endpoint struct {
		proto    string // anthropic | chat | responses (file prefix)
		scenario string // "" | "_tool" | "_thinking" (file suffix)
		baseURL  string
		path     string
		body     []byte
	}
	toolPrompt := JSONQuote("What is the weather in Paris right now? Use the get_weather tool.")
	toolParams := `"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false`
	// Note: responses input is sent in LIST form — strict backends (codex:
	// 400 {"detail":"Input must be a list"}) reject the bare-string shorthand.
	// No max_output_tokens: codex 400s on it ("Unsupported parameter"), and the
	// recording doesn't need a cap — the stream ends naturally.
	respInput := `"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":`
	endpoints := []endpoint{
		{"responses", "", provCfg.OpenAIBaseURL, "/responses", []byte(`{"model":` + qm + `,` + respInput + qp + `}]}],"stream":true,"store":false}`)},
		{"responses", "_tool", provCfg.OpenAIBaseURL, "/responses", []byte(`{"model":` + qm + `,` + respInput + toolPrompt + `}]}],"tools":[{"type":"function","name":"get_weather","description":"Get the current weather for a city","parameters":{` + toolParams + `}}],"tool_choice":"auto","stream":true,"store":false}`)},
		{"responses", "_thinking", provCfg.OpenAIBaseURL, "/responses", []byte(`{"model":` + qm + `,` + respInput + qp + `}]}],"reasoning":{"effort":"low"},"stream":true,"store":false}`)},
		{"chat", "", provCfg.OpenAIBaseURL, "/chat/completions", []byte(`{"model":` + qm + `,"messages":[{"role":"user","content":` + qp + `}],"max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`)},
		{"chat", "_tool", provCfg.OpenAIBaseURL, "/chat/completions", []byte(`{"model":` + qm + `,"messages":[{"role":"user","content":` + toolPrompt + `}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather for a city","parameters":{` + toolParams + `}}}],"tool_choice":"auto","max_tokens":128,"stream":true,"stream_options":{"include_usage":true}}`)},
		{"chat", "_thinking", provCfg.OpenAIBaseURL, "/chat/completions", []byte(`{"model":` + qm + `,"messages":[{"role":"user","content":` + qp + `}],"max_tokens":64,"reasoning_effort":"low","stream":true,"stream_options":{"include_usage":true}}`)},
	}
	anthropicBase := provCfg.AnthropicBaseURL
	if anthropicBase == "" {
		anthropicBase = provCfg.OpenAIBaseURL
	}
	endpoints = append(endpoints,
		endpoint{"anthropic", "", anthropicBase, "/v1/messages", []byte(`{"model":` + qm + `,"max_tokens":64,"messages":[{"role":"user","content":` + qp + `}],"stream":true}`)},
		endpoint{"anthropic", "_tool", anthropicBase, "/v1/messages", []byte(`{"model":` + qm + `,"max_tokens":256,"messages":[{"role":"user","content":` + toolPrompt + `}],"tools":[{"name":"get_weather","description":"Get the current weather for a city","input_schema":{` + toolParams + `}}],"tool_choice":{"type":"auto"},"stream":true}`)},
		// thinking.budget_tokens requires max_tokens > budget.
		endpoint{"anthropic", "_thinking", anthropicBase, "/v1/messages", []byte(`{"model":` + qm + `,"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":` + qp + `}],"stream":true}`)},
	)

	client := &http.Client{Timeout: wireRecordTimeout}
	failed := 0
	for _, ep := range endpoints {
		if ep.baseURL == "" {
			fmt.Printf("  - %s: skipped (no base url)\n", ep.proto)
			continue
		}
		if err := RecordEndpoint(client, provCfg, impl, ep.proto, ep.scenario, ep.baseURL, ep.path, ep.body, provName, outDir); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  %s %s%s: %v\n", provider.Red("✗"), ep.proto, ep.scenario, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d endpoint(s) failed", failed)
	}
	return nil
}

// recordEndpoint streams one minimal request and writes the raw response to
// <out>/<proto>_<provider><scenario>.sse (or .err on non-2xx, without touching
// an existing .sse). The golden replay keys the source protocol off the first
// "_" segment, so proto stays the prefix and scenario the suffix.
func RecordEndpoint(client *http.Client, provCfg configdomain.Provider, impl provider.Provider, proto, scenario, baseURL, path string, body []byte, provName, outDir string) error {
	targetURL := strings.TrimRight(baseURL, "/") + path
	targetURL, body = impl.RewriteRequest(targetURL, body, path)
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if path == "/v1/messages" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if err := impl.AuthHeaders(req); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	for k, v := range provCfg.Headers {
		req.Header.Set(k, v)
	}
	impl.ExtraHeaders(req, path)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	stem := filepath.Join(outDir, proto+"_"+provName+scenario)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		excerpt := raw
		if len(excerpt) > 1<<12 {
			excerpt = excerpt[:1<<12]
		}
		errFile := stem + ".err"
		if err := os.WriteFile(errFile, []byte(fmt.Sprintf("status: %d\n\n%s", resp.StatusCode, excerpt)), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("HTTP %d — wrote %s (existing .sse untouched)", resp.StatusCode, errFile)
	}
	sseFile := stem + ".sse"
	if err := os.WriteFile(sseFile, raw, 0o600); err != nil {
		return err
	}
	fmt.Printf("  %s %s → %s (%d bytes)\n", provider.Green("✓"), proto, sseFile, len(raw))
	return nil
}

// jsonQuote quotes s as a JSON string (strconv.Quote is JSON-compatible for
// these probe bodies).
func JSONQuote(s string) string { return strconv.Quote(s) }

// splitWireRecordArgs separates interspersed flags from positionals (all
// `wire record` flags take a value). Go's flag package alone would stop
// parsing at the first positional.
func SplitWireRecordArgs(args []string) (flagArgs, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
		} else {
			pos = append(pos, a)
		}
	}
	return flagArgs, pos
}

// stripGlobalFlags removes --config/--log-file (and their values) from a flag
// list: those are global flags scanned by configPath / serve startup, never by
// this command's FlagSet.
func StripGlobalFlags(args []string) []string {
	out := args[:0]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" || a == "--log-file" {
			i++ // also skip the value
			continue
		}
		if strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--log-file=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// wireProbeModelLocal picks the model id used in probe bodies: the provider's
// first configured model, else the first route target pointing at it, else a
// derived route target for it (same rule as the wirecap package).
func wireProbeModelLocal(cfg *configdomain.Config, derived map[string][]configdomain.RouteTarget, provName string) string {
	if ms := cfg.Providers[provName].Models; len(ms) > 0 {
		return ms[0]
	}
	for _, targets := range cfg.Routes {
		for _, t := range targets {
			if t.Provider == provName {
				return t.Model
			}
		}
	}
	for _, targets := range derived {
		for _, t := range targets {
			if t.Provider == provName {
				return t.Model
			}
		}
	}
	return ""
}
