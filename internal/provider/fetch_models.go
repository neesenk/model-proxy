package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/upstreamproxy"
	"net/http"
	"strings"
	"time"
)

// fetchModelsBearer is a shared helper for providers that expose an OpenAI-style
// /models endpoint with Bearer auth. Returns the model IDs. Used by aqp,
// zhipu, deepseek — any provider whose AuthHeaders injects a Bearer token and
// whose openai_base_url serves /models.
func fetchModelsBearer(cfg *Config, auth func(*http.Request) error) ([]string, error) {
	url := strings.TrimRight(cfg.OpenAIBaseURL, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if err := auth(req); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: upstreamproxy.AutoTransport()}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch models: HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	ids := make([]string, 0, len(v.Data))
	for _, m := range v.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
