package wirecap

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// ProbeBodies builds the two minimal probe bodies (responses, anthropic) for
// one model id.
func ProbeBodies(model string) (responsesBody, anthropicBody []byte) {
	responsesBody = []byte(`{"model":` + strconv.Quote(model) + `,"input":"hi","max_output_tokens":16,"store":false}`)
	anthropicBody = []byte(`{"model":` + strconv.Quote(model) + `,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	return
}

// Probe sends ONE minimal probe request to the provider's openai_base_url and
// returns the HTTP status (0 on transport/build error). Request build mirrors
// the models-check callable probe (RewriteRequest → AuthHeaders →
// prov.Headers → ExtraHeaders) but ALWAYS uses OpenAIBaseURL — the
// anthropic-base switch is deliberately not replicated: the verdict is about
// what the openai endpoint speaks.
func Probe(client *http.Client, prov configdomain.Provider, impl provider.Provider, path string, body []byte) (int, error) {
	targetURL := strings.TrimRight(prov.OpenAIBaseURL, "/") + path
	targetURL, body = impl.RewriteRequest(targetURL, body, path)
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if path == "/v1/messages" {
		// Anthropic endpoints require the version header; set it BEFORE
		// ExtraHeaders so a provider impl can still override it.
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if err := impl.AuthHeaders(req); err != nil {
		return 0, err
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	impl.ExtraHeaders(req, path)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	return resp.StatusCode, nil
}

// ProbeModel picks the model id used in probe bodies: the provider's first
// configured model, else the first route target pointing at it, else a
// derived route target for it.
func ProbeModel(cfg *configdomain.Config, derived map[string][]configdomain.RouteTarget, provName string) string {
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
