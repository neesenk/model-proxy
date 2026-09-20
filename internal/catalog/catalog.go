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
	Reasoning  bool
	// ReasoningEfforts lists the discrete reasoning-effort levels the model
	// accepts (models.dev reasoning_options type "effort" values). Empty =
	// no effort dial (toggle-only or non-reasoning models). "none" is not an
	// effort level (it is the thinking-off switch) and is filtered out.
	ReasoningEfforts []string
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
	Context   int64    `json:"ctx"`
	Output    int      `json:"out"`
	Input     []string `json:"in"`
	OutMods   []string `json:"out_mod"`
	ToolCall  bool     `json:"tool_call"`
	Reasoning bool     `json:"reasoning"`
	Efforts   []string `json:"efforts,omitempty"`
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

// Names returns every catalog model id, sorted — the picker list for the
// catalog-match UI. A nil catalog yields nil.
func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.byName))
	for name := range c.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ETag returns the last successfully observed HTTP entity tag.
func (c *Catalog) ETag() string {
	if c == nil {
		return ""
	}
	return c.etag
}

// FetchedAt returns when the catalog was last successfully refreshed (zero
// for an in-memory catalog that was never persisted).
func (c *Catalog) FetchedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.fetchedAt
}

// LoadCache reads only the on-disk cache — no network, no TTL check — for
// read-only status surfaces. A missing file is (nil, nil); a corrupt file is
// an error.
func LoadCache(path string) (*Catalog, error) {
	return load(path)
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
	in.ReasoningEfforts = append([]string(nil), in.ReasoningEfforts...)
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
			Reasoning: model.Reasoning, Efforts: append([]string(nil), model.ReasoningEfforts...),
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
		}, ToolCall: model.ToolCall, Reasoning: model.Reasoning, ReasoningEfforts: append([]string(nil), model.Efforts...)}
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
			// models.dev moved tool_call to the model object's top level (the
			// current api.json shape, 7838/7838 models); the nested features
			// shape is kept as a fallback so older responses still parse.
			Features struct {
				ToolCall *bool `json:"tool_call"`
			} `json:"features"`
			ToolCall         *bool `json:"tool_call"`
			Reasoning        *bool `json:"reasoning"`
			ReasoningOptions []struct {
				Type   string   `json:"type"`
				Values []string `json:"values"`
			} `json:"reasoning_options"`
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
			toolCall := source.ToolCall != nil && *source.ToolCall
			if source.ToolCall == nil && source.Features.ToolCall != nil {
				toolCall = *source.Features.ToolCall
			}
			models[name] = Model{
				Context: source.Limit.Context, Output: int(source.Limit.Output),
				Modalities:       Modalities{Input: source.Modalities.Input, Output: source.Modalities.Output},
				ToolCall:         toolCall,
				Reasoning:        source.Reasoning != nil && *source.Reasoning,
				ReasoningEfforts: reasoningEfforts(source.ReasoningOptions),
			}
			ranks[name] = rank
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("models.dev catalog contains no models")
	}
	return New(models), nil
}

// reasoningEfforts projects models.dev reasoning_options onto the discrete
// effort dial: only type "effort" options contribute their values, in the
// order given, deduplicated; "none" is the thinking-off switch, not an
// effort level, and is dropped. Other option types (toggle, budget_tokens)
// carry no effort list.
func reasoningEfforts(options []struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}) []string {
	var out []string
	seen := map[string]bool{}
	for _, opt := range options {
		if opt.Type != "effort" {
			continue
		}
		for _, v := range opt.Values {
			if v == "" || v == "none" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
