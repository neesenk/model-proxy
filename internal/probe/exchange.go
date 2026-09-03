package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// defaultBodyLimit caps the response body read for one probe exchange (enough
// for error-reason excerpts; the wire recorder passes a larger limit).
const defaultBodyLimit = 1 << 14

// Request describes one minimal upstream probe exchange. The CALLER picks the
// base URL (openai vs anthropic) and the path/body shape — Do only executes
// the build+send recipe shared by every probe caller.
type Request struct {
	BaseURL   string
	Method    string // default POST
	Path      string
	Body      []byte
	Accept    string // default application/json (wire recording uses text/event-stream)
	BodyLimit int64  // response read cap; <= 0 → defaultBodyLimit
}

// Reply is the raw outcome of one executed exchange.
type Reply struct {
	Status  int
	Body    []byte // capped at Request.BodyLimit
	Latency time.Duration
}

// Do executes ONE probe exchange: URL join → impl.RewriteRequest → headers
// (Content-Type/Accept/anthropic-version preset on /v1/messages) →
// impl.AuthHeaders → prov.Headers → impl.ExtraHeaders → send. This is the
// single request-build recipe for every probe in the repo (callability,
// protocol matrix, wire recording); classification of the outcome is the
// caller's business.
func Do(ctx context.Context, client *http.Client, prov configdomain.Provider, impl provider.Provider, r Request) (Reply, error) {
	if r.Method == "" {
		r.Method = http.MethodPost
	}
	if r.Accept == "" {
		r.Accept = "application/json"
	}
	limit := r.BodyLimit
	if limit <= 0 {
		limit = defaultBodyLimit
	}

	targetURL := strings.TrimRight(r.BaseURL, "/") + r.Path
	// Provider-specific URL/body tweaks (aqp ?beta=true, codex store:false).
	targetURL, body := impl.RewriteRequest(targetURL, r.Body, r.Path)

	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return Reply{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	// The probe has no client request to copy from, so set only the headers the
	// upstream expects.
	req.Header.Set("Accept", r.Accept)
	if r.Path == "/v1/messages" {
		// Anthropic endpoints require the version header (set BEFORE
		// ExtraHeaders so a provider impl can still override it).
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	if err := impl.AuthHeaders(req); err != nil {
		return Reply{}, fmt.Errorf("auth: %w", err)
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	// Provider-specific per-request headers (aqp: anthropic-version +
	// x-compass-request-id). Same method the forward path calls - one impl.
	impl.ExtraHeaders(req, r.Path)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return Reply{Status: resp.StatusCode, Body: rb, Latency: time.Since(start)}, nil
}

// doCallability executes one callability exchange: Do plus the
// max_completion_tokens retry. Newer OpenAI-shaped models (gpt-5.x behind
// gateways like aqp) reject the legacy max_tokens parameter and demand
// max_completion_tokens instead; that rejection says nothing about
// callability, so the body is retried once with the renamed parameter before
// the verdict.
func doCallability(ctx context.Context, client *http.Client, prov configdomain.Provider, impl provider.Provider, r Request) (Reply, error) {
	rep, err := Do(ctx, client, prov, impl, r)
	if err != nil {
		return rep, err
	}
	if rep.Status >= 200 && rep.Status < 300 {
		return rep, nil
	}
	if wantsMaxCompletionTokens(rep.Body) {
		if retryBody, ok := swapMaxTokensParam(r.Body); ok {
			r.Body = retryBody
			if rep2, err2 := Do(ctx, client, prov, impl, r); err2 == nil {
				return rep2, nil
			}
		}
	}
	return rep, nil
}

// wantsMaxCompletionTokens reports whether an upstream error body rejects the
// probe's max_tokens parameter in favor of max_completion_tokens (the shape
// newer OpenAI reasoning models require, e.g. gpt-5.x behind aqp's gateway:
// "Unsupported parameter: 'max_tokens' ... Use 'max_completion_tokens'
// instead."). The string only appears in that suggestion, so a plain Contains
// is a sufficient signal.
func wantsMaxCompletionTokens(body []byte) bool {
	return strings.Contains(string(body), "max_completion_tokens")
}

// swapMaxTokensParam renames the max_tokens key to max_completion_tokens in a
// JSON request body, preserving the value. ok=false when the body isn't a JSON
// object carrying max_tokens (e.g. codex's /responses probe, which never sends
// it) - in that case no retry is attempted.
func swapMaxTokensParam(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false
	}
	v, ok := m["max_tokens"]
	if !ok {
		return nil, false
	}
	delete(m, "max_tokens")
	m["max_completion_tokens"] = v
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}
