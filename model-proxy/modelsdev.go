package main

import (
	"encoding/json"
	"net/url"
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
