package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// modelsdev.go — auto-source model metadata (context/output/modalities) from
// https://models.dev/api.json. The catalog is fetched once, deduplicated, and
// cached as a slim projection (~150 KB) at ~/.model-proxy/models_cache.json
// (24h TTL, ETag 304-aware). Effective metadata resolves config > models.dev >
// default; models.dev only supplements gaps (route models missing from config),
// NEVER overrides config, and config.yaml is never written. Scope: the models +
// takeover CLI paths only — the daemon/proxy hot path is unaffected.

// modelsDevModel is the slim per-model metadata projection cached from
// models.dev/api.json (only the fields model-proxy uses). Compact json keys keep
// the cache file small.
type modelsDevModel struct {
	Context int64    `json:"ctx"`
	Output  int      `json:"out"`
	Input   []string `json:"in"`
	OutMods []string `json:"out_mod"`
}

// toProviderModel converts the slim projection into a config ProviderModel.
func (m modelsDevModel) toProviderModel() ProviderModel {
	return ProviderModel{
		Context:    m.Context,
		Output:     m.Output,
		Modalities: ProviderModalities{Input: m.Input, Output: m.OutMods},
	}
}

// modelsDevCatalog is the on-disk + in-memory cache: a deduplicated name→metadata
// index plus an endpoint→model-names index for endpoint-scoped matching.
type modelsDevCatalog struct {
	FetchedAt  time.Time                 `json:"fetched_at"`
	Etag       string                    `json:"etag"`
	ByName     map[string]modelsDevModel `json:"by_name"`
	ByEndpoint map[string][]string       `json:"by_endpoint"`
}

// modelSource records where a model's effective metadata came from.
type modelSource int

const (
	srcConfig    modelSource = iota // present in config models: block
	srcModelsDev                    // matched in the models.dev catalog
	srcDefault                      // unmatched → defaultProviderModel
)

// defaultProviderModel is written for models found in neither config nor the
// catalog, so a takeover client config never gets a zero/nonsense limit.
var defaultProviderModel = ProviderModel{
	Context:    200000,
	Output:     16384,
	Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}},
}

// canonicalModelsDevOwners are models.dev provider keys that own a model family
// outright (rank 0); all other providers (resellers/aggregators) are rank 1. On
// a name collision the canonical owner's metadata wins the deduped ByName entry.
var canonicalModelsDevOwners = map[string]bool{
	"zhipuai": true, "deepseek": true, "openai": true, "anthropic": true,
	"google": true, "moonshotai": true, "xai": true, "mistral": true,
	"cohere": true, "meta": true, "alibaba": true, "minimax": true,
	"stepfun": true, "amazon-bedrock": true,
}

func ownerRank(providerKey string) int {
	if canonicalModelsDevOwners[providerKey] {
		return 0
	}
	return 1
}

// normalizeEndpoint lowercases and strips a trailing slash for stable matching.
func normalizeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")
	return strings.ToLower(raw)
}

// hostOf returns the lowercased host of a URL string, or "" if unparseable.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// parseModelsDevAPI parses a models.dev/api.json blob into a deduplicated catalog.
// Canonical owners win on name collisions. Each provider's models are indexed
// under both the full-normalized api URL and its host.
func parseModelsDevAPI(blob []byte) *modelsDevCatalog {
	var raw map[string]struct {
		API    string `json:"api"`
		Models map[string]struct {
			Limit struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limit"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(blob, &raw); err != nil {
		return emptyCatalog()
	}
	cat := &modelsDevCatalog{ByName: map[string]modelsDevModel{}, ByEndpoint: map[string][]string{}}
	rank := map[string]int{} // modelName → current owner rank (lower wins)
	for provKey, p := range raw {
		r := ownerRank(provKey)
		epFull := normalizeEndpoint(p.API)
		epHost := hostOf(epFull)
		names := make([]string, 0, len(p.Models))
		for modelName, m := range p.Models {
			names = append(names, modelName)
			md := modelsDevModel{
				Context: m.Limit.Context,
				Output:  int(m.Limit.Output),
				Input:   m.Modalities.Input,
				OutMods: m.Modalities.Output,
			}
			if cur, ok := rank[modelName]; !ok || r < cur {
				cat.ByName[modelName] = md
				rank[modelName] = r
			}
		}
		sort.Strings(names)
		if epFull != "" {
			cat.ByEndpoint[epFull] = appendUnique(cat.ByEndpoint[epFull], names)
		}
		if epHost != "" && epHost != epFull {
			cat.ByEndpoint[epHost] = appendUnique(cat.ByEndpoint[epHost], names)
		}
	}
	return cat
}

// appendUnique appends names not already present in dst.
func appendUnique(dst, add []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, x := range dst {
		seen[x] = true
	}
	for _, x := range add {
		if !seen[x] {
			dst = append(dst, x)
			seen[x] = true
		}
	}
	return dst
}

func emptyCatalog() *modelsDevCatalog {
	return &modelsDevCatalog{ByName: map[string]modelsDevModel{}, ByEndpoint: map[string][]string{}}
}

const catalogTTL = 24 * time.Hour

const defaultModelsDevEndpoint = "https://models.dev/api.json"

// modelsDevEndpoint returns the catalog endpoint, overridable via MP_MODELSDEV_URL
// (tests / self-hosted mirrors).
func modelsDevEndpoint() string {
	if v := envOrEmpty("MP_MODELSDEV_URL"); v != "" {
		return v
	}
	return defaultModelsDevEndpoint
}

// cachePath is the on-disk catalog cache location.
func cachePath() string {
	return filepath.Join(homeDir(), ".model-proxy", "models_cache.json")
}

// catalogFetchFunc fetches the catalog given an endpoint + last-known etag.
// Returns HTTP status, body (nil on 304), the response etag, and any error.
type catalogFetchFunc func(endpoint, etag string) (status int, body []byte, newEtag string, err error)

// realModelsDevFetch is the production fetch. Do NOT set Accept-Encoding manually
// — Go's Transport auto-requests gzip and transparently decompresses (a manual
// header disables auto-decompress), so the 3 MB body transfers as ~286 KB.
// If-None-Match yields a free 304 when the catalog is unchanged.
var realModelsDevFetch catalogFetchFunc = func(endpoint, etag string) (int, []byte, string, error) {
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

// loadCachedCatalog reads the cache file; returns (nil, nil) if absent.
func loadCachedCatalog(path string) (*modelsDevCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cat modelsDevCatalog
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, err
	}
	if cat.ByName == nil {
		cat.ByName = map[string]modelsDevModel{}
	}
	if cat.ByEndpoint == nil {
		cat.ByEndpoint = map[string][]string{}
	}
	return &cat, nil
}

// saveCachedCatalog atomically writes the cache.
func saveCachedCatalog(path string, cat *modelsDevCatalog) error {
	data, err := json.Marshal(cat)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// ensureCatalogFresh returns a usable catalog, fetching from models.dev only when
// the cache is missing/stale (or force=true). 304 refreshes fetched_at only;
// 200 rebuilds + persists; a fetch error falls back to a stale cache (logged to
// stderr) or an empty catalog so the calling command still runs.
func ensureCatalogFresh(cacheFile, endpoint string, fetch catalogFetchFunc, force bool) (*modelsDevCatalog, error) {
	cached, _ := loadCachedCatalog(cacheFile)
	if !force && cached != nil && time.Since(cached.FetchedAt) < catalogTTL {
		return cached, nil
	}
	etag := ""
	if cached != nil {
		etag = cached.Etag
	}
	status, body, newEtag, err := fetch(endpoint, etag)
	if err != nil {
		if cached != nil {
			fmt.Fprintf(os.Stderr, "model-proxy: models.dev unreachable (%v); using catalog cached %s ago\n", err, ageString(cached.FetchedAt))
			return cached, nil
		}
		return emptyCatalog(), nil
	}
	switch status {
	case http.StatusNotModified:
		cached.FetchedAt = time.Now()
		if newEtag != "" {
			cached.Etag = newEtag
		}
		_ = saveCachedCatalog(cacheFile, cached)
		return cached, nil
	case http.StatusOK:
		cat := parseModelsDevAPI(body)
		cat.FetchedAt = time.Now()
		cat.Etag = newEtag
		_ = saveCachedCatalog(cacheFile, cat)
		return cat, nil
	default:
		if cached != nil {
			return cached, nil
		}
		return emptyCatalog(), nil
	}
}

// ageString renders a duration since t as e.g. "5h" / "3m" / "just now".
func ageString(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// lookup resolves a model's metadata given the provider's base URLs (openai +
// anthropic). It tries endpoint-scoped matching first (full URL, then host) so a
// provider's own models.dev entry wins; then falls back to a global name match
// (rescuing models a provider borrows from another vendor). ok=false if neither.
func (cat *modelsDevCatalog) lookup(endpoints []string, model string) (modelsDevModel, bool) {
	if cat == nil {
		return modelsDevModel{}, false
	}
	for _, e := range endpoints {
		ne := normalizeEndpoint(e)
		for _, key := range []string{ne, hostOf(ne)} {
			if key == "" {
				continue
			}
			names, ok := cat.ByEndpoint[key]
			if !ok {
				continue
			}
			for _, n := range names {
				if n == model {
					md, ok := cat.ByName[model]
					return md, ok // endpoint-scoped hit
				}
			}
		}
	}
	if md, ok := cat.ByName[model]; ok {
		return md, true // global name fallback
	}
	return modelsDevModel{}, false
}
