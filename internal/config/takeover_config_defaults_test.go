package config

import (
	"strings"
	"testing"
)

// --- takeover block tombstone: the key was removed; a config still carrying
// it must fail to load with a migration hint (templates own client targets) ---

func TestTakeoverBlock_RemovedTombstone(t *testing.T) {
	_, err := LoadConfigFromBytes("test", []byte(`
listen: 127.0.0.1:15721
takeover:
  claude: ~/.claude/settings.json
  provider_id: model-proxy
providers:
  aqp:
    provider_id: aqp
    openai_base_url: https://example.invalid/compass-api/v1
    models:
      - glm-5.2
`))
	if err == nil || !strings.Contains(err.Error(), "takeover: block is no longer supported") {
		t.Errorf("takeover block tombstone: want migration error, got %v", err)
	}
}

// An empty/absent takeover block stays loadable.
func TestTakeoverBlock_AbsentLoads(t *testing.T) {
	if _, err := LoadConfigFromBytes("test", []byte(`
listen: 127.0.0.1:15721
providers:
  aqp:
    provider_id: aqp
    openai_base_url: https://example.invalid/compass-api/v1
    models:
      - glm-5.2
`)); err != nil {
		t.Errorf("no takeover block: load must succeed, got %v", err)
	}
}
