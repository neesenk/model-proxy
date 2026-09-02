package routing

import (
	"testing"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// TestHydrateModels verifies the routing policy that combines configured
// provider and route names with catalog metadata and the routing-owned
// fallback/source rules.
func TestHydrateModels(t *testing.T) {
	cat := catalog.New(map[string]catalog.Model{
		"glm-4.6":         {Context: 204800, Output: 131072, Modalities: catalog.Modalities{Input: []string{"text"}, Output: []string{"text"}}},
		"deepseek-v4-pro": {Context: 1000000, Output: 65536, Modalities: catalog.Modalities{Input: []string{"text"}, Output: []string{"text"}}},
	})
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "https://open.bigmodel.cn/api/paas/v4",
				Models: []string{"glm-4.6"}},
			"aqp":        {OpenAIBaseURL: "https://compass.llm.shopee.io/compass-api/v1"},
			"codex":      {OpenAIBaseURL: "https://chatgpt.com/backend-api/codex"},
			"volcengine": {Models: []string{"doubao-x"}},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm-4.6":         {{Provider: "zhipu", Model: "glm-4.6", Priority: 1}},
			"deepseek-v4-pro": {{Provider: "aqp", Model: "deepseek-v4-pro", Priority: 1}},
			"gpt-5.5":         {{Provider: "codex", Model: "gpt-5.5", Priority: 1}},
		},
	}
	meta, src := HydrateModels(cfg, cat)

	if pm := meta["zhipu"]["glm-4.6"]; pm.Context != 204800 || pm.Output != 131072 {
		t.Errorf("zhipu/glm-4.6 should be catalog-sourced: %+v", pm)
	}
	if src["zhipu"]["glm-4.6"] != SrcModelsDev {
		t.Errorf("zhipu/glm-4.6 source = %v, want SrcModelsDev", src["zhipu"]["glm-4.6"])
	}
	if pm := meta["aqp"]["deepseek-v4-pro"]; pm.Context != 1000000 || pm.Output != 65536 {
		t.Errorf("aqp/deepseek-v4-pro should be catalog-sourced: %+v", pm)
	}
	if src["aqp"]["deepseek-v4-pro"] != SrcModelsDev {
		t.Errorf("aqp/deepseek-v4-pro source = %v, want SrcModelsDev", src["aqp"]["deepseek-v4-pro"])
	}
	if pm := meta["codex"]["gpt-5.5"]; pm.Context != 200000 || pm.Output != 16384 {
		t.Errorf("codex/gpt-5.5 should be default: %+v", pm)
	}
	if src["codex"]["gpt-5.5"] != SrcDefault {
		t.Errorf("codex/gpt-5.5 source = %v, want SrcDefault", src["codex"]["gpt-5.5"])
	}
	if pm, ok := meta["volcengine"]["doubao-x"]; !ok || pm.Context != 200000 {
		t.Errorf("volcengine/doubao-x should be present + default: %+v ok=%v", pm, ok)
	}
	if src["volcengine"]["doubao-x"] != SrcDefault {
		t.Errorf("volcengine/doubao-x source = %v, want SrcDefault", src["volcengine"]["doubao-x"])
	}
}
