// Package probe owns ALL provider probe execution: the single request-build +
// send recipe (Do), the model callability probe (Callable/Exchange) used by
// CLI (`model-proxy test`, `models refresh`) and the Web active-probe port,
// and the three-protocol matrix probe (ProbeModelProtocols) shared by the
// daemon's startup capability probing, `models refresh` and `wire record`.
// Provider construction and credential binding stay in the application;
// verdict classification and storage live in internal/runtime/wirecap — this
// package only executes exchanges and reports raw outcomes.
package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
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
// AuthHeaders, ExtraHeaders). A non-2xx that rejects `max_tokens` in favor of
// `max_completion_tokens` (newer OpenAI-shaped models) triggers ONE retry with
// the renamed parameter before the model is judged uncallable. A
// build/auth/network error is reported as
// ok=false with reason set (status 0) — such a model is conservatively
// dropped by callers that filter model lists.
func Callable(ctx context.Context, client *http.Client, prov configdomain.Provider, impl provider.Provider, modelID string) (ok bool, status int, reason string) {
	// Ask the provider implementation for its probe request shape (path + body).
	// Each provider owns its probe path/body in its own file.
	pr := impl.ProbeRequest(modelID)

	// Select the base URL by protocol, mirroring forward. A provider with an
	// anthropic_base_url is probed over the anthropic protocol (its primary chat
	// path for Claude Code); otherwise the openai protocol. A pure-decisions
	// provider (typesafe: no chat bases) is probed on its decisions base with
	// the impl's own ProbeRequest shape (System One), never rewritten.
	baseURL := prov.OpenAIBaseURL
	path, body := pr.Path, pr.Body
	switch {
	case prov.AnthropicBaseURL != "":
		baseURL = prov.AnthropicBaseURL
		// The impl's ProbeRequest may be openai-shaped; on the anthropic base
		// the probe must speak anthropic (path + body), otherwise strict bases
		// 404 (deepseek) and lenient ones get tested with the wrong protocol
		// shape (zhipu's gateway accepts /chat/completions on the anthropic
		// base).
		path = "/v1/messages"
		body = provider.AnthropicProbeBody(modelID)
	case baseURL == "" && prov.DecisionsBaseURL != "":
		baseURL = prov.DecisionsBaseURL
	}

	rep, err := doCallability(ctx, client, prov, impl, Request{
		BaseURL: baseURL,
		Method:  pr.Method,
		Path:    path,
		Body:    body,
	})
	if err != nil {
		return false, 0, err.Error()
	}
	if rep.Status >= 200 && rep.Status < 300 {
		return true, rep.Status, ""
	}
	return false, rep.Status, reasonForBody(rep.Body)
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
