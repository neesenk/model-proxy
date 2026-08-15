package catalog

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Modalities records the catalog-declared input and output media.
type Modalities struct {
	Input  []string
	Output []string
}

// Model is metadata supplied by models.dev for one globally named model.
type Model struct {
	Context    int64
	Output     int
	Modalities Modalities
	ToolCall   bool
}

// Catalog is an immutable, globally name-keyed models.dev projection.
type Catalog struct {
	fetchedAt time.Time
	etag      string
	byName    map[string]Model
}

type diskCatalog struct {
	FetchedAt time.Time            `json:"fetched_at"`
	ETag      string               `json:"etag"`
	ByName    map[string]diskModel `json:"by_name"`
}

type diskModel struct {
	Context  int64    `json:"ctx"`
	Output   int      `json:"out"`
	Input    []string `json:"in"`
	OutMods  []string `json:"out_mod"`
	ToolCall bool     `json:"tool_call"`
}

// New creates a catalog from name-keyed metadata. Both the map and modality
// slices are copied, so later caller mutations cannot alter the catalog.
func New(models map[string]Model) *Catalog {
	return &Catalog{byName: cloneModels(models)}
}

// Lookup returns a defensive copy of metadata for model.
func (c *Catalog) Lookup(model string) (Model, bool) {
	if c == nil {
		return Model{}, false
	}
	m, ok := c.byName[model]
	return cloneModel(m), ok
}

// Count returns the number of distinct globally named models.
func (c *Catalog) Count() int {
	if c == nil {
		return 0
	}
	return len(c.byName)
}

// ETag returns the last successfully observed HTTP entity tag.
func (c *Catalog) ETag() string {
	if c == nil {
		return ""
	}
	return c.etag
}

func empty() *Catalog { return New(nil) }

func cloneModels(in map[string]Model) map[string]Model {
	out := make(map[string]Model, len(in))
	for name, model := range in {
		out[name] = cloneModel(model)
	}
	return out
}

func cloneModel(in Model) Model {
	in.Modalities.Input = append([]string(nil), in.Modalities.Input...)
	in.Modalities.Output = append([]string(nil), in.Modalities.Output...)
	return in
}

func (c *Catalog) marshalJSON() ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("nil catalog")
	}
	byName := make(map[string]diskModel, len(c.byName))
	for name, model := range c.byName {
		byName[name] = diskModel{
			Context: model.Context, Output: model.Output,
			Input:   append([]string(nil), model.Modalities.Input...),
			OutMods: append([]string(nil), model.Modalities.Output...), ToolCall: model.ToolCall,
		}
	}
	return json.Marshal(diskCatalog{FetchedAt: c.fetchedAt, ETag: c.etag, ByName: byName})
}

func unmarshalCatalog(data []byte) (*Catalog, error) {
	var disk diskCatalog
	if err := json.Unmarshal(data, &disk); err != nil {
		return nil, err
	}
	if len(disk.ByName) == 0 {
		return nil, fmt.Errorf("cached models.dev catalog contains no models")
	}
	models := make(map[string]Model, len(disk.ByName))
	for name, model := range disk.ByName {
		models[name] = Model{Context: model.Context, Output: model.Output, Modalities: Modalities{
			Input: append([]string(nil), model.Input...), Output: append([]string(nil), model.OutMods...),
		}, ToolCall: model.ToolCall}
	}
	return &Catalog{fetchedAt: disk.FetchedAt, etag: disk.ETag, byName: models}, nil
}

var canonicalOwners = map[string]bool{
	"zhipuai": true, "deepseek": true, "openai": true, "anthropic": true,
	"google": true, "moonshotai": true, "xai": true, "mistral": true,
	"cohere": true, "meta": true, "alibaba": true, "minimax": true,
	"stepfun": true, "amazon-bedrock": true,
}

func ownerRank(provider string) int {
	if canonicalOwners[provider] {
		return 0
	}
	return 1
}

// parse converts a models.dev API response into the compact global-name index.
// Canonical providers win name collisions; among equal ranks the FIRST provider
// in sorted provider-name order wins, so map iteration order can never change
// the projection. (Equal-rank collisions retain the sorted-first value, the
// deterministic reading of the historical "retain the first value" rule.)
func parse(data []byte) (*Catalog, error) {
	var raw map[string]struct {
		Models map[string]struct {
			Limit struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limit"`
			Modalities struct {
				Input  []string `json:"input"`
				Output []string `json:"output"`
			} `json:"modalities"`
			Features struct {
				ToolCall *bool `json:"tool_call"`
			} `json:"features"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("models.dev catalog contains no providers")
	}
	providers := make([]string, 0, len(raw))
	for provider := range raw {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	models := make(map[string]Model)
	ranks := make(map[string]int)
	for _, provider := range providers {
		rank := ownerRank(provider)
		for name, source := range raw[provider].Models {
			if current, ok := ranks[name]; ok && rank >= current {
				continue
			}
			models[name] = Model{
				Context: source.Limit.Context, Output: int(source.Limit.Output),
				Modalities: Modalities{Input: source.Modalities.Input, Output: source.Modalities.Output},
				ToolCall:   source.Features.ToolCall != nil && *source.Features.ToolCall,
			}
			ranks[name] = rank
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models.dev catalog contains no models")
	}
	return New(models), nil
}
