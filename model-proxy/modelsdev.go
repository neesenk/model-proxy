package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
// index (canonical-owner dedup at parse time resolves cross-provider name clashes).
type modelsDevCatalog struct {
	FetchedAt time.Time                 `json:"fetched_at"`
	Etag      string                    `json:"etag"`
	ByName    map[string]modelsDevModel `json:"by_name"`
}

// modelSource records where a model's effective metadata came from (config now
// carries only names, so there is no srcConfig — metadata is always runtime).
type modelSource int

const (
	srcModelsDev modelSource = iota // matched in the models.dev catalog
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

// parseModelsDevAPI parses a models.dev/api.json blob into a deduplicated catalog.
// Canonical owners win on name collisions.
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
	cat := &modelsDevCatalog{ByName: map[string]modelsDevModel{}}
	rank := map[string]int{} // modelName → current owner rank (lower wins)
	for provKey, p := range raw {
		r := ownerRank(provKey)
		for modelName, m := range p.Models {
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
	}
	return cat
}

func emptyCatalog() *modelsDevCatalog {
	return &modelsDevCatalog{ByName: map[string]modelsDevModel{}}
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
		// Total failure: no cache + unreachable. Return an error so `models pull`
		// surfaces it instead of printing a false "refreshed: 0 unique models".
		// Callers that only need a best-effort catalog (models/takeover display)
		// discard the error and fall back to the empty catalog.
		return emptyCatalog(), fmt.Errorf("models.dev unreachable and no cached catalog: %w", err)
	}
	switch status {
	case http.StatusNotModified:
		// 304 requires a prior cache (we sent its etag). A 304 with no cache is an
		// anomalous intermediary response — don't deref nil; fall back to empty.
		if cached == nil {
			return emptyCatalog(), nil
		}
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

// hydrateModels computes the effective metadata for every model a provider serves
// — the union of the config `Models` name list and any model named in `routes` —
// resolving context/output/modalities from the models.dev catalog (srcModelsDev)
// or conservative defaults (srcDefault). Config carries only names now, so there
// is no "config metadata" source. Returns two parallel maps keyed provider→model.
// cfg and config.yaml are not mutated.
func hydrateModels(cfg *Config, cat *modelsDevCatalog) (meta map[string]map[string]ProviderModel, sources map[string]map[string]modelSource) {
	meta = map[string]map[string]ProviderModel{}
	sources = map[string]map[string]modelSource{}
	ensure := func(prov string) (map[string]ProviderModel, map[string]modelSource) {
		if meta[prov] == nil {
			meta[prov] = map[string]ProviderModel{}
			sources[prov] = map[string]modelSource{}
		}
		return meta[prov], sources[prov]
	}
	resolve := func(prov Provider, name string) (ProviderModel, modelSource) {
		if md, ok := cat.lookup(name); ok {
			return md.toProviderModel(), srcModelsDev
		}
		return defaultProviderModel, srcDefault
	}
	// 1. config name list.
	for provName, prov := range cfg.Providers {
		m, s := ensure(provName)
		for _, name := range prov.Models {
			if _, dup := m[name]; dup {
				continue
			}
			m[name], s[name] = resolve(prov, name)
		}
	}
	// 2. route model names not already covered (config ∪ routes).
	for _, targets := range cfg.Routes {
		for _, t := range targets {
			prov, ok := cfg.Providers[t.Provider]
			if !ok {
				continue
			}
			m, s := ensure(t.Provider)
			if _, exists := m[t.Model]; exists {
				continue
			}
			m[t.Model], s[t.Model] = resolve(prov, t.Model)
		}
	}
	return meta, sources
}

// lookup resolves a model's metadata from the deduplicated catalog. ByName
// already holds the canonical owner's entry (canonical-owner dedup at parse
// time), so this is a plain global name lookup; a provider borrowing another
// vendor's model name still resolves to the canonical metadata. ok=false if
// the name isn't in the catalog.
func (cat *modelsDevCatalog) lookup(model string) (modelsDevModel, bool) {
	if cat == nil {
		return modelsDevModel{}, false
	}
	md, ok := cat.ByName[model]
	return md, ok
}
