package app

import (
	configdomain "model-proxy/internal/config"
	"testing"

	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
)

// Pool virtual names (name#id) normalize to the parent's provider id before
// the dialect lookup (providerConfig resolves via parentOf).
func TestParity_ReasoningDialectPooledProvider(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://x"}},
	}
	parentOf := map[string]string{"zhipu#ab12": "zhipu"}
	prov, ok := configdomain.ProviderConfig(cfg, parentOf, "zhipu#ab12")
	if !ok {
		t.Fatal("configdomain.ProviderConfig did not resolve pooled virtual")
	}
	out, err := protocol.ConvertRequestWithOptions(
		[]byte(`{"model":"g","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		protocol.Responses,
		protocol.OpenAI,
		protocol.RequestOptions{
			ImageOK:          true,
			ReasoningDialect: protocol.ReasoningDialect(provider.ChatReasoningMode(prov.Provider)),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strOf(asMap(unmarshalMap(t, out)["thinking"])["type"]); got != "enabled" {
		t.Errorf("pooled zhipu#ab12 → thinking = %v, want enabled (parent id zhipu)", got)
	}
}
