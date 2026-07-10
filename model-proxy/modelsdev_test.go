package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureAPI is a tiny models.dev/api.json-shaped blob. Both the canonical
// owner (zhipuai) and a reseller (openrouter) list bare "glm-4.6" — same key —
// so the dedup test exercises canonical-owner preference. deepseek lists a
// distinct model. Used across the parse/lookup/hydrate tests.
const fixtureAPI = `{
  "zhipuai": {
    "api": "https://open.bigmodel.cn/api/paas/v4",
    "models": {
      "glm-4.6": {"limit": {"context": 204800, "output": 131072}, "modalities": {"input": ["text"], "output": ["text"]}}
    }
  },
  "openrouter": {
    "api": "https://openrouter.ai/api/v1",
    "models": {
      "glm-4.6": {"limit": {"context": 999, "output": 999}, "modalities": {"input": ["text"], "output": ["text"]}}
    }
  },
  "deepseek": {
    "api": "https://api.deepseek.com",
    "models": {
      "deepseek-v4-pro": {"limit": {"context": 1000000, "output": 65536}, "modalities": {"input": ["text"], "output": ["text"]}}
    }
  }
}`

func TestParseModelsDevAPI_DedupAndEndpoints(t *testing.T) {
	cat := parseModelsDevAPI([]byte(fixtureAPI))
	// ByName: glm-4.6 deduped — canonical owner zhipuai wins (ctx 204800, not 999).
	md, ok := cat.ByName["glm-4.6"]
	if !ok {
		t.Fatal("glm-4.6 missing from ByName")
	}
	if md.Context != 204800 || md.Output != 131072 {
		t.Errorf("glm-4.6 canonical owner not preferred: got ctx=%d out=%d", md.Context, md.Output)
	}
	if len(cat.ByName) != 2 {
		t.Errorf("ByName dedup count = %d, want 2 (glm-4.6, deepseek-v4-pro); got %v", len(cat.ByName), catalogKeys(cat.ByName))
	}
	// deepseek-v4-pro present with its real limits.
	if md2 := cat.ByName["deepseek-v4-pro"]; md2.Context != 1000000 || md2.Output != 65536 {
		t.Errorf("deepseek-v4-pro metadata wrong: %+v", md2)
	}
	// ByEndpoint: zhipu's exact URL + host key both index glm-4.6.
	zhipuURL := normalizeEndpoint("https://open.bigmodel.cn/api/paas/v4")
	if names, ok := cat.ByEndpoint[zhipuURL]; !ok || !sliceContains(names, "glm-4.6") {
		t.Errorf("ByEndpoint[%s] missing glm-4.6: %v", zhipuURL, names)
	}
	if names, ok := cat.ByEndpoint["open.bigmodel.cn"]; !ok || !sliceContains(names, "glm-4.6") {
		t.Errorf("host-only endpoint key missing glm-4.6: %v", names)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"https://open.bigmodel.cn/api/paas/v4": "https://open.bigmodel.cn/api/paas/v4",
		"https://API.DeepSeek.com/":            "https://api.deepseek.com",
		"":                                     "",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

// helpers
func catalogKeys(m map[string]modelsDevModel) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// keep imports used by later-appended tests honest.
var (
	_ = os.ReadFile
	_ = filepath.Join
	_ = time.Now
)

func TestCatalogLookup(t *testing.T) {
	cat := parseModelsDevAPI([]byte(fixtureAPI))

	// 1. Endpoint match: zhipu base URL → zhipuai provider → glm-4.6 (canonical).
	md, ok := cat.lookup([]string{"https://open.bigmodel.cn/api/paas/v4"}, "glm-4.6")
	if !ok || md.Context != 204800 {
		t.Errorf("endpoint match glm-4.6: ok=%v ctx=%d", ok, md.Context)
	}
	// 2. Name fallback: aqp endpoint has no models.dev entry; deepseek-v4-pro
	//    still resolves globally.
	md, ok = cat.lookup([]string{"https://compass.llm.shopee.io/compass-api/v1"}, "deepseek-v4-pro")
	if !ok || md.Context != 1000000 {
		t.Errorf("name fallback deepseek-v4-pro: ok=%v ctx=%d", ok, md.Context)
	}
	// 3. Name fallback returns canonical value even without an endpoint hit
	//    (reseller's 999 must not leak through global lookup).
	md, ok = cat.lookup(nil, "glm-4.6")
	if !ok || md.Context != 204800 {
		t.Errorf("global name lookup should give canonical: ok=%v ctx=%d", ok, md.Context)
	}
	// 4. Unmatched (no endpoint, no global name) → ok=false.
	if _, ok := cat.lookup([]string{"https://chatgpt.com/backend-api/codex"}, "gpt-5.5"); ok {
		t.Error("gpt-5.5 should be unmatched")
	}
	// 5. nil catalog never panics.
	var nilCat *modelsDevCatalog
	if _, ok := nilCat.lookup([]string{"http://x"}, "m"); ok {
		t.Error("nil catalog lookup should return false")
	}
}
