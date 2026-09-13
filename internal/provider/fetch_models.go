package provider

import (
	"context"
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
//
// ModelInfo / fetchModelInfosBearer below are the display-name-aware
// projection of the same endpoint.
func fetchModelsBearer(cfg *Config, auth func(*http.Request) error) ([]string, error) {
	return fetchModelsBearerContext(context.Background(), cfg, auth)
}

// fetchModelsBearerContext is the cancellable ids projection behind the
// FetchModelsContext methods (models_context.go): daemon refreshes must not
// detach an uncancellable fetch into an unowned goroutine.
func fetchModelsBearerContext(ctx context.Context, cfg *Config, auth func(*http.Request) error) ([]string, error) {
	infos, err := fetchModelInfosBearerContext(ctx, cfg, auth)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(infos))
	for _, m := range infos {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// ModelInfo is one /models entry projected for presentation: the API id plus
// the upstream's display name when it reports one. Kimi Code is the canonical
// case — it serves "K2.8 Preview" under the stable id `kimi-for-coding`, so
// the id set alone cannot reveal that the model behind an id changed; the
// display name is exactly what the vendor CLI shows its users.
type ModelInfo struct {
	ID          string
	DisplayName string
}

// ModelInfoLister is the optional FetchModels upgrade for providers whose
// /models reports presentation names. When a provider implements it,
// `models refresh` surfaces the upstream display names (stderr per-model
// lines + the kept table's NAME column) so an id-stable/model-swapped
// upstream cannot hide behind unchanged ids. Providers without the capability
// degrade to plain ids with empty display names.
type ModelInfoLister interface {
	FetchModelInfos() ([]ModelInfo, error)
}

// fetchModelInfosBearer is fetchModelsBearer's display-name-aware sibling:
// the same OpenAI-style {data:[{id, display_name?}]} endpoint, projected with
// presentation fields. display_name is optional (plain OpenAI /models omits
// it; the field is simply empty then).
func fetchModelInfosBearer(cfg *Config, auth func(*http.Request) error) ([]ModelInfo, error) {
	return fetchModelInfosBearerContext(context.Background(), cfg, auth)
}

func fetchModelInfosBearerContext(ctx context.Context, cfg *Config, auth func(*http.Request) error) ([]ModelInfo, error) {
	url := strings.TrimRight(cfg.OpenAIBaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("parse models response: %w", err)
	}
	infos := make([]ModelInfo, 0, len(v.Data))
	for _, m := range v.Data {
		infos = append(infos, ModelInfo{ID: m.ID, DisplayName: m.DisplayName})
	}
	return infos, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
