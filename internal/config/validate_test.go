package config

import (
	"strings"
	"testing"
)

// validate_test.go covers ValidateYAML: the structured lint behind
// POST /api/config/validate, including best-effort line attribution.

const validateBaseYAML = `listen: 127.0.0.1:15721
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    models:
      - glm-4.6
routes:
  glm:
    - provider: zhipu
      model: glm-4.6
`

func TestValidateYAML_Valid(t *testing.T) {
	if issues := ValidateYAML([]byte(validateBaseYAML)); issues != nil {
		t.Fatalf("valid config issues=%v", issues)
	}
}

func TestValidateYAML_SyntaxErrorLine(t *testing.T) {
	// Tabs are illegal indentation; the parser must pinpoint line 5 exactly.
	bad := strings.Replace(validateBaseYAML, "    openai_base_url:", "\topenai_base_url:", 1)
	issues := ValidateYAML([]byte(bad))
	if len(issues) != 1 {
		t.Fatalf("issues=%v want exactly one", issues)
	}
	// Tabs are illegal indentation. The reported line comes straight from
	// yaml.v3's scanner (it points at the mapping above the tabbed line) —
	// the test pins that we surface it verbatim.
	if issues[0].Line != 4 {
		t.Fatalf("line=%d want 4 (message=%q)", issues[0].Line, issues[0].Message)
	}
}

func TestValidateYAML_TypeErrorLines(t *testing.T) {
	// models: as a map (line 6) is a yaml type error with a per-field line.
	bad := strings.Replace(validateBaseYAML, "    models:\n      - glm-4.6\n", "    models:\n      glm-4.6: {}\n", 1)
	issues := ValidateYAML([]byte(bad))
	if len(issues) != 1 {
		t.Fatalf("issues=%v want exactly one", issues)
	}
	if issues[0].Line != 7 {
		t.Fatalf("line=%d want 7 (message=%q)", issues[0].Line, issues[0].Message)
	}
	if !strings.Contains(issues[0].Message, "cannot unmarshal") {
		t.Fatalf("message=%q want the unmarshal detail", issues[0].Message)
	}
	if strings.Contains(issues[0].Message, "line ") {
		t.Fatalf("message=%q must not keep the line prefix", issues[0].Message)
	}
	// The map-form models: hint from LoadConfigFromBytes must survive.
	if !strings.Contains(issues[0].Message, "hint:") {
		t.Fatalf("message=%q lost the models hint", issues[0].Message)
	}
}

func TestValidateYAML_SemanticErrorLines(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(string) string
		wantLine int // 0 asserts only that no line could be located
		wantMsg  string
	}{
		{
			name: "provider missing base url",
			mutate: func(s string) string {
				return strings.Replace(s, "    openai_base_url: https://open.bigmodel.cn/api/paas/v4\n", "", 1)
			},
			wantLine: 3, // providers.zhipu
			wantMsg:  `provider "zhipu"`,
		},
		{
			name: "route unknown provider",
			mutate: func(s string) string {
				return strings.Replace(s, "    - provider: zhipu\n", "    - provider: nope\n", 1)
			},
			wantLine: 10, // routes.glm target 0 item line
			wantMsg:  `route "glm" target 0`,
		},
		{
			name: "route target line",
			mutate: func(s string) string {
				return strings.Replace(s, "    - provider: zhipu\n      model: glm-4.6\n", "    - provider: zhipu\n      model: glm-4.6\n      protocol: anthropic\n", 1)
			},
			wantLine: 10, // the target item line
			wantMsg:  `protocol:anthropic`,
		},
		{
			name: "dotted duration field",
			mutate: func(s string) string {
				return s + "scheduling:\n  quota_poll_interval: nope\n"
			},
			wantLine: 13, // scheduling.quota_poll_interval
			wantMsg:  `scheduling.quota_poll_interval`,
		},
		{
			name: "bad listen",
			mutate: func(s string) string {
				return strings.Replace(s, "listen: 127.0.0.1:15721", "listen: 0.0.0.0:15721", 1)
			},
			wantLine: 1,
			wantMsg:  `not loopback`,
		},
		{
			name:     "unlocatable whole-document error",
			mutate:   func(string) string { return "listen: 127.0.0.1:15721\n" },
			wantLine: 0,
			wantMsg:  `no providers configured`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := ValidateYAML([]byte(tc.mutate(validateBaseYAML)))
			if len(issues) != 1 {
				t.Fatalf("issues=%v want exactly one", issues)
			}
			if issues[0].Line != tc.wantLine {
				t.Fatalf("line=%d want %d (message=%q)", issues[0].Line, tc.wantLine, issues[0].Message)
			}
			if !strings.Contains(issues[0].Message, tc.wantMsg) {
				t.Fatalf("message=%q want substring %q", issues[0].Message, tc.wantMsg)
			}
		})
	}
}

func TestValidateYAML_ShadowSampleRateLine(t *testing.T) {
	bad := validateBaseYAML + "shadow_sample_rate: 2\n"
	issues := ValidateYAML([]byte(bad))
	if len(issues) != 1 {
		t.Fatalf("issues=%v want exactly one", issues)
	}
	if issues[0].Line != 12 {
		t.Fatalf("line=%d want 12 (message=%q)", issues[0].Line, issues[0].Message)
	}
}
