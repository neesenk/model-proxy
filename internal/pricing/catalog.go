// Package pricing owns the price-catalog domain used by analytics.
//
// Catalog prices and computed costs use USD per token. Config-facing
// overrides use USD per million tokens and are converted only at Resolve.
package pricing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultEndpoint is the OpenRouter model catalog used for price discovery.
	DefaultEndpoint = "https://openrouter.ai/api/v1/models"
	// DefaultTTL is the default lifetime of a persisted price catalog.
	DefaultTTL = 24 * time.Hour
)

// Entry is a model price in USD per token.
type Entry struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Catalog is the persisted and in-memory price index keyed by bare model name.
type Catalog struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Etag      string           `json:"etag"`
	ByModel   map[string]Entry `json:"by_model"`
}

// Override is a config-provided model price in USD per million tokens.
type Override struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Empty returns a usable catalog with no known prices.
func Empty() *Catalog {
	return &Catalog{ByModel: map[string]Entry{}}
}

// canonicalVendors own a model family outright. Their entry wins when a
// reseller exposes the same bare model name.
var canonicalVendors = map[string]bool{
	"openai": true, "anthropic": true, "google": true, "deepseek": true,
	"z-ai": true, "moonshotai": true, "xai": true, "mistral": true,
	"cohere": true, "meta": true, "alibaba": true, "minimax": true,
	"qwen": true, "stepfun": true, "amazon-bedrock": true, "bedrock": true,
	"zhipuai": true,
}

func vendorRank(id string) int {
	if i := strings.IndexByte(id, '/'); i > 0 && canonicalVendors[id[:i]] {
		return 0
	}
	return 1
}

func bareModelName(id string) string {
	id = strings.TrimPrefix(id, "~")
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	return id
}

func parsePrice(s string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return value
}

// parseOpenRouter decodes an OpenRouter pricing payload. A malformed body or
// an empty data array is an ERROR, never an empty catalog: the 200 path in
// EnsureFresh persists the parsed result, and persisting an empty catalog
// would poison a previously good cache until upstream changes its ETag.
func parseOpenRouter(blob []byte) (*Catalog, error) {
	var raw struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return Empty(), fmt.Errorf("pricing source returned malformed JSON: %w", err)
	}
	if len(raw.Data) == 0 {
		return Empty(), fmt.Errorf("pricing source returned an empty model list")
	}

	catalog := Empty()
	ranks := make(map[string]int)
	for _, model := range raw.Data {
		name := bareModelName(model.ID)
		if name == "" {
			continue
		}
		entry := Entry{
			Prompt:     parsePrice(model.Pricing.Prompt),
			Completion: parsePrice(model.Pricing.Completion),
			CacheRead:  parsePrice(model.Pricing.InputCacheRead),
			CacheWrite: parsePrice(model.Pricing.InputCacheWrite),
		}
		rank := vendorRank(model.ID)
		if current, exists := ranks[name]; !exists || rank < current {
			catalog.ByModel[name] = entry
			ranks[name] = rank
		}
	}
	if len(catalog.ByModel) == 0 {
		return Empty(), fmt.Errorf("pricing source listed no usable models")
	}
	return catalog, nil
}

// Lookup returns the exact bare-name catalog entry. It is nil-safe.
func (catalog *Catalog) Lookup(model string) (Entry, bool) {
	if catalog == nil {
		return Entry{}, false
	}
	entry, ok := catalog.ByModel[model]
	return entry, ok
}

// Resolve applies a config override before falling back to the catalog.
func Resolve(overrides map[string]Override, catalog *Catalog, model string) (Entry, bool) {
	if override, ok := overrides[model]; ok {
		return Entry{
			Prompt:     override.Input / 1e6,
			Completion: override.Output / 1e6,
			CacheRead:  override.CacheRead / 1e6,
			CacheWrite: override.CacheWrite / 1e6,
		}, true
	}
	return catalog.Lookup(model)
}

// ComputeCost prices one analytics bucket in USD.
func ComputeCost(input, output, cacheRead, cacheCreation uint64, entry Entry) float64 {
	return float64(input)*entry.Prompt +
		float64(output)*entry.Completion +
		float64(cacheRead)*entry.CacheRead +
		float64(cacheCreation)*entry.CacheWrite
}
