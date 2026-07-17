package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pricing.go — auto-source per-model pricing (USD/token) from OpenRouter's
// /api/v1/models catalog, cached like models.dev (24h TTL, ETag/304, offline
// fallback). Used only to compute equivalent pay-as-you-go cost for the
// analytics dashboard; real spend is not tracked. Prices are never fabricated:
// an unknown model is reported unpriced.

// pricingEntry is the per-model price in USD per token.
type pricingEntry struct {
	Prompt     float64 `json:"prompt"`      // input
	Completion float64 `json:"completion"`  // output
	CacheRead  float64 `json:"cache_read"`  // cached-input read
	CacheWrite float64 `json:"cache_write"` // cache-creation write
}

// pricingCatalog is the on-disk + in-memory price index keyed by bare model name.
type pricingCatalog struct {
	FetchedAt time.Time               `json:"fetched_at"`
	Etag      string                  `json:"etag"`
	ByModel   map[string]pricingEntry `json:"by_model"`
}

func emptyPricingCatalog() *pricingCatalog {
	return &pricingCatalog{ByModel: map[string]pricingEntry{}}
}

// canonicalORVendors are OpenRouter vendor prefixes that own a model family
// outright (rank 0); resellers/aggregators are rank 1. On a bare-name collision
// the canonical vendor's price wins.
var canonicalORVendors = map[string]bool{
	"openai": true, "anthropic": true, "google": true, "deepseek": true,
	"z-ai": true, "moonshotai": true, "xai": true, "mistral": true,
	"cohere": true, "meta": true, "alibaba": true, "minimax": true,
	"qwen": true, "stepfun": true, "amazon-bedrock": true, "bedrock": true,
	"zhipuai": true,
}

func orVendorRank(id string) int {
	if i := strings.IndexByte(id, '/'); i > 0 {
		if canonicalORVendors[id[:i]] {
			return 0
		}
	}
	return 1
}

// bareModelName strips a leading "~" (OpenRouter dynamic alias) and the
// "vendor/" prefix from an OpenRouter id, returning the bare model name.
func bareModelName(id string) string {
	id = strings.TrimPrefix(id, "~")
	if i := strings.IndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// atof is a forgiving string→float64 (OpenRouter prices are decimal strings);
// unparseable → 0.
func atof(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// parsePricingAPI parses an OpenRouter /api/v1/models blob into a deduplicated
// bare-name → price catalog. Canonical vendor wins on a bare-name collision.
func parsePricingAPI(blob []byte) *pricingCatalog {
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
		return emptyPricingCatalog()
	}
	cat := emptyPricingCatalog()
	rank := map[string]int{}
	for _, m := range raw.Data {
		name := bareModelName(m.ID)
		if name == "" {
			continue
		}
		e := pricingEntry{
			Prompt:     atof(m.Pricing.Prompt),
			Completion: atof(m.Pricing.Completion),
			CacheRead:  atof(m.Pricing.InputCacheRead),
			CacheWrite: atof(m.Pricing.InputCacheWrite),
		}
		r := orVendorRank(m.ID)
		if cur, ok := rank[name]; !ok || r < cur {
			cat.ByModel[name] = e
			rank[name] = r
		}
	}
	return cat
}

// lookup resolves a model's price by bare name. ok=false if absent. Nil-safe.
func (cat *pricingCatalog) lookup(model string) (pricingEntry, bool) {
	if cat == nil {
		return pricingEntry{}, false
	}
	e, ok := cat.ByModel[model]
	return e, ok
}

const pricingTTL = 24 * time.Hour

const defaultPricingEndpoint = "https://openrouter.ai/api/v1/models"

// pricingEndpoint returns the catalog endpoint, overridable via MP_PRICING_URL.
func pricingEndpoint() string {
	if v := envOrEmpty("MP_PRICING_URL"); v != "" {
		return v
	}
	return defaultPricingEndpoint
}

// pricingCachePath is the on-disk catalog cache location.
func pricingCachePath() string {
	return filepath.Join(homeDir(), ".model-proxy", "pricing_cache.json")
}

// pricingFetchFunc mirrors catalogFetchFunc (modelsdev.go).
type pricingFetchFunc func(endpoint, etag string) (status int, body []byte, newEtag string, err error)

// realPricingFetch is the production fetch. Do NOT set Accept-Encoding manually
// (Go Transport auto-gzipes + decompresses).
var realPricingFetch pricingFetchFunc = func(endpoint, etag string) (int, []byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	newEtag := resp.Header.Get("ETag")
	if resp.StatusCode == http.StatusNotModified {
		return http.StatusNotModified, nil, newEtag, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, newEtag, err
	}
	return resp.StatusCode, body, newEtag, nil
}

func loadCachedPricing(path string) (*pricingCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cat pricingCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	if cat.ByModel == nil {
		cat.ByModel = map[string]pricingEntry{}
	}
	return &cat, nil
}

func saveCachedPricing(path string, cat *pricingCatalog) error {
	data, err := json.Marshal(cat)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// ensurePricingFresh mirrors ensureCatalogFresh: fresh→use; stale/force→fetch
// (304 refreshes fetched_at, 200 rebuilds+persists); fetch error falls back to a
// stale cache (logged) or an empty catalog. ttl overrides the 24h default.
func ensurePricingFresh(cacheFile, endpoint string, fetch pricingFetchFunc, force bool, ttl time.Duration) (*pricingCatalog, error) {
	cached, _ := loadCachedPricing(cacheFile)
	if !force && cached != nil && time.Since(cached.FetchedAt) < ttl {
		return cached, nil
	}
	etag := ""
	if cached != nil {
		etag = cached.Etag
	}
	status, body, newEtag, err := fetch(endpoint, etag)
	if err != nil {
		if cached != nil {
			fmt.Fprintf(os.Stderr, "model-proxy: pricing source unreachable (%v); using catalog cached %s ago\n", err, ageString(cached.FetchedAt))
			return cached, nil
		}
		return emptyPricingCatalog(), fmt.Errorf("pricing source unreachable and no cached catalog: %w", err)
	}
	switch status {
	case http.StatusNotModified:
		if cached == nil {
			return emptyPricingCatalog(), nil
		}
		cached.FetchedAt = time.Now()
		if newEtag != "" {
			cached.Etag = newEtag
		}
		_ = saveCachedPricing(cacheFile, cached)
		return cached, nil
	case http.StatusOK:
		cat := parsePricingAPI(body)
		cat.FetchedAt = time.Now()
		cat.Etag = newEtag
		_ = saveCachedPricing(cacheFile, cat)
		return cat, nil
	default:
		if cached != nil {
			return cached, nil
		}
		return emptyPricingCatalog(), nil
	}
}
