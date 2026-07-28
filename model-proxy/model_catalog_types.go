package main

// ProviderModel is runtime metadata resolved from the models.dev catalog. It
// is deliberately separate from the YAML configuration domain.
type ProviderModel struct {
	Context    int64
	Output     int
	Modalities ProviderModalities
	ToolCall   bool
}

// ProviderModalities records the catalog-declared input and output media.
type ProviderModalities struct {
	Input  []string
	Output []string
}
