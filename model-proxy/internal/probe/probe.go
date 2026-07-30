// Package probe owns minimal provider callability probes shared by CLI
// (`model-proxy test`, `models refresh`, `wire`) and the Web active-probe
// port. Provider construction and credential binding stay in the application;
// this package only executes one HTTP exchange per target.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/provider"
)

// Result is one probe exchange outcome. Status 0 means the request never
// reached an HTTP exchange (build/auth/network error) — Reason explains it.
type Result struct {
	OK      bool
	Status  int
	Reason  string
	Latency time.Duration
}

// Callable sends a minimal request for modelID to the provider's configured
// base_url, using that provider's call rules (protocol/path/auth/rewrite/
// extra-headers via the provider implementation), and returns whether the
// upstream answered 2xx. prov is the provider CONFIG (base urls, headers);
// impl is the provider IMPLEMENTATION (ProbeRequest, RewriteRequest,
// AuthHeaders, ExtraHeaders). A build/auth/network error is reported as
// ok=false with reason set (status 0) — such a model is conservatively
// dropped by callers that filter model lists.
func Callable(ctx context.Context, client *http.Client, prov configdomain.Provider, impl provider.Provider, modelID string) (ok bool, status int, reason string) {
	// Ask the provider implementation for its probe request shape (path + body).
	// Each provider owns its probe path/body in its own file.
	pr := impl.ProbeRequest(modelID)
	if pr.Method == "" {
		pr.Method = http.MethodPost
	}

	// Select the base URL by protocol, mirroring forward. A provider with an
	// anthropic_base_url is probed over the anthropic protocol (its primary chat
	// path for Claude Code); otherwise the openai protocol.
	baseURL := prov.OpenAIBaseURL
	path, body := pr.Path, pr.Body
	if prov.AnthropicBaseURL != "" {
		baseURL = prov.AnthropicBaseURL
		// The impl's ProbeRequest is openai-shaped; on the anthropic base the
		// probe must speak anthropic (path + body), otherwise strict bases 404
		// (deepseek) and lenient ones get tested with the wrong protocol shape
		// (zhipu's gateway accepts /chat/completions on the anthropic base).
		path = "/v1/messages"
		body = []byte(`{"model":` + strconv.Quote(modelID) + `,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	}

	targetURL := strings.TrimRight(baseURL, "/") + path
	// Provider-specific URL/body tweaks (aqp ?beta=true, codex store:false).
	targetURL, body = impl.RewriteRequest(targetURL, body, path)

	req, err := http.NewRequestWithContext(ctx, pr.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return false, 0, "build request: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	// The probe has no client request to copy from, so set only the headers the
	// upstream expects.
	req.Header.Set("Accept", "application/json")
	if path == "/v1/messages" {
		// Anthropic endpoints require the version header (set BEFORE
		// ExtraHeaders so a provider impl can still override it).
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	if err := impl.AuthHeaders(req); err != nil {
		return false, 0, "auth: " + err.Error()
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	// Provider-specific per-request headers (aqp: anthropic-version +
	// x-compass-request-id). Same method the forward path calls - one impl.
	impl.ExtraHeaders(req, path)

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, "request: " + err.Error()
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14)) // 16KB cap for the reason excerpt

	status = resp.StatusCode
	if status >= 200 && status < 300 {
		return true, status, ""
	}
	return false, status, reasonForBody(rb)
}

// Exchange is the latency-timed form of Callable.
func Exchange(ctx context.Context, client *http.Client, prov configdomain.Provider, impl provider.Provider, modelID string) Result {
	start := time.Now()
	ok, status, r := Callable(ctx, client, prov, impl, modelID)
	return Result{OK: ok, Status: status, Reason: r, Latency: time.Since(start)}
}

// Reason extracts a short "code: message" from an OpenAI-style error body,
// falling back to the raw body (truncated) when the shape doesn't parse.
func reasonForBody(body []byte) string {
	var ej struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &ej) == nil && ej.Error != nil {
		code := ej.Error.Code
		if code == "" {
			code = ej.Error.Type
		}
		// The upstream appends a long, useless "Request id: <hex>" to the
		// message - strip it so the drop summary stays readable.
		msg := strings.TrimSpace(StripRequestID(ej.Error.Message))
		if code != "" && msg != "" {
			return code + ": " + msg
		}
		if code != "" {
			return code
		}
		if msg != "" {
			return msg
		}
	}
	s := strings.TrimSpace(StripRequestID(string(body)))
	return truncate(s, 160)
}

// StripRequestID removes a trailing " Request id: <token>" (case-insensitive)
// from an upstream error message. Volcengine appends this to every error body;
// it's noise in the filter summary.
func StripRequestID(s string) string {
	if i := strings.LastIndex(strings.ToLower(s), " request id:"); i >= 0 {
		return strings.TrimRight(s[:i], " ")
	}
	return s
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
