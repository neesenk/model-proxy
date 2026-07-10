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
