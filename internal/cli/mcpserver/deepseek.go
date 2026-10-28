package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	mcpkg "model-proxy/internal/mcp"
)

// deepseek.go — the `deepseek-search` bundled MCP server: DeepSeek's Web
// Search/Fetch tool API (POST /v1/tools/search, POST /v1/tools/fetch; Bearer
// auth, per https://api-docs.deepseek.com/zh-cn/guides/search/) exposed as
// two stdio MCP tools. The server is a strict sequential JSON-RPC loop (one
// in-flight call — the MCP stdio model); every request is answered with
// exactly one line. Contract violations answer JSON-RPC -32602; upstream
// failures return a normal tools/call result with isError=true so route
// failover (which only fails over on transport faults) never retries a
// billed call that already reached DeepSeek.

const (
	// deepseekDefaultBaseURL is the documented API base. DEEPSEEK_BASE_URL
	// overrides it (tests, diagnostics); it must NOT carry /v1 — the tool
	// paths below append it.
	deepseekDefaultBaseURL = "https://api.deepseek.com"
	// deepseekAPIKeyEnv is the credential the gateway injects from the
	// provider pool via env: {DEEPSEEK_API_KEY: "${account.api_key}"}.
	deepseekAPIKeyEnv = "DEEPSEEK_API_KEY"
	// deepseekBaseURLEnv is the benign base-URL override knob.
	deepseekBaseURLEnv = "DEEPSEEK_BASE_URL"
	// deepseekServerVersion is the bundled server's own version (bumped with
	// the tool surface; unrelated to the proxy release version).
	deepseekServerVersion = "1.0"
	// deepseekCallTimeout bounds one upstream tool call (docs bill only
	// successful requests; a hung call must not park the stdio loop when the
	// server also runs standalone, outside the gateway's `timeout:`).
	deepseekCallTimeout = 60 * time.Second
	// deepseekMaxResponseBytes bounds one API response read.
	deepseekMaxResponseBytes = 32 << 20
)

// ServeDeepSeek runs the deepseek-search MCP stdio server until stdin EOF.
// Configuration comes from the process environment (the gateway's stdio
// children get a minimal env plus the configured entries).
func ServeDeepSeek(stdin io.Reader, stdout io.Writer) error {
	base := strings.TrimRight(os.Getenv(deepseekBaseURLEnv), "/")
	if base == "" {
		base = deepseekDefaultBaseURL
	}
	srv := &deepseekServer{
		client: &http.Client{Timeout: deepseekCallTimeout}, // DefaultTransport: env-proxy honored
		base:   base,
		key:    os.Getenv(deepseekAPIKeyEnv),
	}
	out := bufio.NewWriter(stdout)
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // same bound as mcpkg.StdioConn
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		resp := srv.dispatch(line)
		if resp == nil {
			continue // notification or response: never answered
		}
		if _, err := out.Write(append(resp, '\n')); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
		if err := out.Flush(); err != nil {
			return fmt.Errorf("flush response: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// dispatch answers one JSON-RPC line (nil = stay silent). Malformed JSON and
// batches get protocol-level errors; valid requests are routed by method.
func (s *deepseekServer) dispatch(line []byte) []byte {
	if !json.Valid(line) {
		return mcpkg.BuildErrorResponse(nil, -32700, "parse error: invalid JSON")
	}
	frame := mcpkg.ParseFrame(line)
	if frame.IsBatch {
		// MCP 2025-03-26 dropped batching; the gateway never sends one.
		return mcpkg.BuildErrorResponse(nil, -32600, "invalid request: JSON-RPC batches are not supported")
	}
	if !frame.HasID || frame.IsResponse {
		return nil
	}
	switch frame.Method {
	case "initialize":
		// Echo the client's protocol version (server is version-agnostic);
		// the gateway stores the negotiated value for the session.
		return mcpkg.BuildResultResponse(frame.ID, s.initializeResult(mcpkg.ParseClientProtocol(line)))
	case "notifications/initialized":
		return nil
	case "ping":
		return mcpkg.BuildResultResponse(frame.ID, json.RawMessage(`{}`))
	case "tools/list":
		return mcpkg.BuildResultResponse(frame.ID, s.toolsListResult())
	case "tools/call":
		return s.toolCall(frame.ID, line)
	case "":
		return mcpkg.BuildErrorResponse(frame.ID, -32600, "invalid request: missing method")
	default:
		return mcpkg.BuildErrorResponse(frame.ID, -32601, fmt.Sprintf("method %q not found", frame.Method))
	}
}

func (s *deepseekServer) initializeResult(clientProtocol string) json.RawMessage {
	if clientProtocol == "" {
		clientProtocol = "2025-03-26"
	}
	out := map[string]any{
		"protocolVersion": clientProtocol,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "model-proxy-deepseek-search", "version": deepseekServerVersion},
	}
	data, _ := json.Marshal(out)
	return data
}

// toolsListResult is the static tool surface: schemas mirror the DeepSeek
// Search/Fetch API contracts (query 1-128 chars + count 1-50; url http(s)).
func (s *deepseekServer) toolsListResult() json.RawMessage {
	tools := []mcpkg.ToolSpec{
		{
			Name:        "web_search",
			Description: "Web search via DeepSeek. Supports search operators (site:/-site:, lang:, after:, before:, quoted phrases, -exclusions). Results are rerank-scored; each carries title, url, site, date, snippet and a model-friendly summary.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"search query, 1-128 unicode characters; supports site:/lang:/after:/before: operators, quoted phrases and -exclusions"},"count":{"type":"integer","description":"number of results, 1-50 (default 10)"},"need_content":{"type":"boolean","description":"include each result's full page text (default false — the summary usually suffices)"}},"required":["query"]}`),
		},
		{
			Name:        "url_fetch",
			Description: "Fetch one web page via DeepSeek and return its title and cleaned markdown content.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"absolute http(s) URL to fetch (no credentials in URL)"}},"required":["url"]}`),
		},
	}
	data, _ := json.Marshal(map[string]any{"tools": tools})
	return data
}

// toolCall executes one tools/call request. Schema violations answer -32602
// (before any network egress — an invalid call must not be billed); upstream
// failures answer isError tool results.
func (s *deepseekServer) toolCall(id json.RawMessage, line []byte) []byte {
	var req struct {
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil || req.Params.Name == "" {
		return mcpkg.BuildErrorResponse(id, -32602, "invalid params: tools/call needs params.name")
	}
	var args map[string]json.RawMessage
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return mcpkg.BuildErrorResponse(id, -32602, "invalid params: arguments must be an object")
		}
	}
	switch req.Params.Name {
	case "web_search":
		return s.callSearch(id, args)
	case "url_fetch":
		return s.callFetch(id, args)
	default:
		return mcpkg.BuildErrorResponse(id, -32602, fmt.Sprintf("unknown tool: %s", req.Params.Name))
	}
}

func (s *deepseekServer) callSearch(id json.RawMessage, args map[string]json.RawMessage) []byte {
	query, errResp := stringArg(args, "query", 1, 128)
	if errResp != nil {
		return mcpkg.BuildErrorResponse(id, -32602, *errResp)
	}
	count := 0
	if raw, ok := args["count"]; ok {
		if err := json.Unmarshal(raw, &count); err != nil || count < 1 || count > 50 {
			return mcpkg.BuildErrorResponse(id, -32602, "invalid params: count must be an integer 1-50")
		}
	}
	needContent := false
	if raw, ok := args["need_content"]; ok {
		if err := json.Unmarshal(raw, &needContent); err != nil {
			return mcpkg.BuildErrorResponse(id, -32602, "invalid params: need_content must be a boolean")
		}
	}
	if s.key == "" {
		return s.toolError(id, "web_search", deepseekMissingKeyMessage())
	}
	results, err := s.search(context.Background(), query, count, needContent)
	if err != nil {
		return s.toolError(id, "web_search", err.Error())
	}
	return mcpkg.BuildResultResponse(id, toolText(renderSearchResults(query, results)))
}

func (s *deepseekServer) callFetch(id json.RawMessage, args map[string]json.RawMessage) []byte {
	rawURL, errResp := stringArg(args, "url", 1, 8192)
	if errResp != nil {
		return mcpkg.BuildErrorResponse(id, -32602, *errResp)
	}
	if msg := validateFetchURL(rawURL); msg != "" {
		return mcpkg.BuildErrorResponse(id, -32602, "invalid params: "+msg)
	}
	if s.key == "" {
		return s.toolError(id, "url_fetch", deepseekMissingKeyMessage())
	}
	title, content, err := s.fetch(context.Background(), rawURL)
	if err != nil {
		return s.toolError(id, "url_fetch", err.Error())
	}
	text := content
	if title != "" {
		text = "# " + title + "\n\n" + content
	}
	return mcpkg.BuildResultResponse(id, toolText(text))
}

func deepseekMissingKeyMessage() string {
	return fmt.Sprintf("%s is not set — the gateway injects it via env: {%s: \"${account.api_key}\"} from the deepseek provider pool; standalone runs must export it", deepseekAPIKeyEnv, deepseekAPIKeyEnv)
}

// stringArg extracts one required string argument with a length window.
func stringArg(args map[string]json.RawMessage, name string, minLen, maxLen int) (string, *string) {
	raw, ok := args[name]
	if !ok {
		msg := "invalid params: " + name + " is required"
		return "", &msg
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		msg := "invalid params: " + name + " must be a string"
		return "", &msg
	}
	if n := len([]rune(v)); n < minLen || n > maxLen {
		msg := "invalid params: " + name + fmt.Sprintf(" must be %d-%d characters", minLen, maxLen)
		return "", &msg
	}
	return v, nil
}

// validateFetchURL enforces the documented fetch contract client-side so an
// invalid target fails before any billed egress: http(s), host present, no
// userinfo.
func validateFetchURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "url must be an absolute http(s) URL"
	}
	if u.User != nil {
		return "url must not carry credentials (user:pass)"
	}
	return ""
}

// toolError renders a failed tool execution: a normal result with isError
// true (the MCP convention — protocol-level errors are for contract faults).
func (s *deepseekServer) toolError(id json.RawMessage, tool, message string) []byte {
	payload := map[string]any{
		"content": []map[string]string{{"type": "text", "text": "deepseek " + tool + " failed: " + message}},
		"isError": true,
	}
	data, _ := json.Marshal(payload)
	return mcpkg.BuildResultResponse(id, data)
}

func toolText(text string) json.RawMessage {
	payload := map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
	data, _ := json.Marshal(payload)
	return data
}

// deepseekServer is one stdio session's DeepSeek API client state.
type deepseekServer struct {
	client *http.Client
	base   string
	key    string
}

// searchResult mirrors one /v1/tools/search result entry. Content-heavy
// fields stay optional: need_content=false omits content server-side.
type searchResult struct {
	RerankScore float64 `json:"rerank_score"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Content     string  `json:"content"`
	SiteName    string  `json:"site_name"`
	Date        string  `json:"date"`
}

func (s *deepseekServer) search(ctx context.Context, query string, count int, needContent bool) ([]searchResult, error) {
	body := map[string]any{"query": query}
	if count > 0 {
		body["count"] = count
	}
	if needContent {
		body["need_content"] = true
	}
	var out struct {
		Results []searchResult `json:"results"`
	}
	if err := s.postJSON(ctx, "/v1/tools/search", body, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

func (s *deepseekServer) fetch(ctx context.Context, rawURL string) (title, content string, err error) {
	body := map[string]any{"url": rawURL}
	var out struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := s.postJSON(ctx, "/v1/tools/fetch", body, &out); err != nil {
		return "", "", err
	}
	return out.Title, out.Content, nil
}

// postJSON sends one Bearer-authenticated JSON POST and decodes the bounded
// response into out. Non-2xx answers carry the OpenAI-style error message
// when present.
func (s *deepseekServer) postJSON(ctx context.Context, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, deepseekMaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(data) > deepseekMaxResponseBytes {
		return fmt.Errorf("response exceeds %d bytes", deepseekMaxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, apiErrorMessage(data))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// apiErrorMessage extracts {error:{message}} from an OpenAI-style error
// body; anything else falls back to a short raw prefix.
func apiErrorMessage(data []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &v); err == nil && v.Error.Message != "" {
		if code, ok := v.Error.Code.(string); ok && code != "" {
			return v.Error.Message + " (code " + code + ")"
		}
		return v.Error.Message
	}
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) > 200 {
		trimmed = trimmed[:200]
	}
	if trimmed == "" {
		trimmed = "<empty body>"
	}
	return trimmed
}

// renderSearchResults formats results as compact text for model consumption:
// one numbered block per result, summary preferred over snippet, content only
// when the caller asked for it.
func renderSearchResults(query string, results []searchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d result%s for %q (DeepSeek)\n", len(results), plural(len(results)), query)
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, orDash(r.Title))
		fmt.Fprintf(&b, "   %s\n", orDash(r.URL))
		meta := []string{}
		if r.SiteName != "" {
			meta = append(meta, r.SiteName)
		}
		if r.Date != "" {
			meta = append(meta, r.Date)
		}
		if r.RerankScore > 0 {
			meta = append(meta, fmt.Sprintf("score %.1f", r.RerankScore))
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "   %s\n", strings.Join(meta, " · "))
		}
		text := r.Summary
		if text == "" {
			text = r.Snippet
		}
		if text != "" {
			fmt.Fprintf(&b, "   %s\n", text)
		}
		if r.Content != "" {
			fmt.Fprintf(&b, "   content: %s\n", r.Content)
		}
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
