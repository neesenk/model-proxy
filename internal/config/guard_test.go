package config

import (
	"strings"
	"testing"
)

const guardTestBaseYAML = `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
`

func TestGuardSecretsDefaultIsLog(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Guard.SecretsAction(); got != "log" {
		t.Errorf("SecretsAction() = %q, want default %q", got, "log")
	}
}

func TestGuardSecretsValidActions(t *testing.T) {
	for _, action := range []string{"log", "redact", "block", "off"} {
		cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {secrets: "+action+"}\n"))
		if err != nil {
			t.Fatalf("secrets=%s: %v", action, err)
		}
		if got := cfg.Guard.SecretsAction(); got != action {
			t.Errorf("SecretsAction() = %q, want %q", got, action)
		}
	}
}

func TestGuardSecretsInvalidAction(t *testing.T) {
	_, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {secrets: drop}\n"))
	if err == nil || !strings.Contains(err.Error(), "guard.secrets") {
		t.Errorf("invalid action: err = %v, want a guard.secrets error", err)
	}
}

func TestGuardNewFieldDefaults(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Guard.KnownSecretsEnabled() {
		t.Error("KnownSecretsEnabled() = false, want default true")
	}
	if !cfg.Guard.DecodeEnabled() {
		t.Error("DecodeEnabled() = false, want default true")
	}
	if !cfg.Guard.AuditEnabled() {
		t.Error("AuditEnabled() = false, want default true")
	}
	if got := cfg.Guard.PathsAction(); got != "log" {
		t.Errorf("PathsAction() = %q, want default %q", got, "log")
	}
	if got, want := cfg.Guard.AuditPathValue("/home/u"), "/home/u/.model-proxy/security.log"; got != want {
		t.Errorf("AuditPathValue() = %q, want derived default %q", got, want)
	}
}

// A guard block that sets only some keys must keep the load-time defaults of
// the others (the rawConfig default literal pattern, same as web.enabled).
func TestGuardDefaultsSurvivePartialBlock(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {secrets: block}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Guard.KnownSecretsEnabled() || !cfg.Guard.DecodeEnabled() || !cfg.Guard.AuditEnabled() {
		t.Errorf("partial guard block lost defaults: %+v", cfg.Guard)
	}
	if got := cfg.Guard.PathsAction(); got != "log" {
		t.Errorf("PathsAction() = %q, want %q", got, "log")
	}
}

func TestGuardExplicitFalseHonored(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+
		"guard: {known_secrets: false, decode: false, audit: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Guard.KnownSecretsEnabled() || cfg.Guard.DecodeEnabled() || cfg.Guard.AuditEnabled() {
		t.Errorf("explicit false not honored: %+v", cfg.Guard)
	}
}

func TestGuardPathsValidActions(t *testing.T) {
	for _, action := range []string{"log", "block", "off"} {
		cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {paths: "+action+"}\n"))
		if err != nil {
			t.Fatalf("paths=%s: %v", action, err)
		}
		if got := cfg.Guard.PathsAction(); got != action {
			t.Errorf("PathsAction() = %q, want %q", got, action)
		}
	}
}

func TestGuardPathsInvalidActions(t *testing.T) {
	for _, action := range []string{"redact", "drop"} {
		_, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {paths: "+action+"}\n"))
		if err == nil || !strings.Contains(err.Error(), "guard.paths") {
			t.Errorf("paths=%s: err = %v, want a guard.paths error", action, err)
		}
	}
	// redact specifically must explain why it is unsupported.
	_, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {paths: redact}\n"))
	if err == nil || !strings.Contains(err.Error(), "redact is not supported") {
		t.Errorf("paths=redact: err = %v, want an explanation that redact is unsupported", err)
	}
}

func TestGuardAuditPathValidation(t *testing.T) {
	_, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {audit_path: logs/security.log}\n"))
	if err == nil || !strings.Contains(err.Error(), "guard.audit_path") {
		t.Errorf("relative audit_path: err = %v, want a guard.audit_path error", err)
	}
	cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {audit_path: /var/log/security.log}\n"))
	if err != nil {
		t.Fatalf("absolute audit_path: %v", err)
	}
	if got := cfg.Guard.AuditPathValue("/home/u"); got != "/var/log/security.log" {
		t.Errorf("AuditPathValue() = %q, want configured path", got)
	}
}

func TestGuardExtraPatternsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		wantErr string
	}{
		{"bad name", `{name: My-Key!, regex: 'mv-[a-z]+'}`, "name"},
		{"empty name", `{name: '', regex: 'mv-[a-z]+'}`, "name"},
		{"name too long", `{name: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, regex: 'mv-[a-z]+'}`, "name"},
		{"empty regex", `{name: myvendor_key, regex: ''}`, "regex"},
		{"uncompilable regex", `{name: myvendor_key, regex: 'mv-([a-z]+'}`, "does not compile"},
		{"literal not guaranteed", `{name: myvendor_key, regex: 'mv-[0-9]{32}', literal: 'mv-x'}`, "guaranteed substring"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := guardTestBaseYAML + "guard:\n  extra_patterns:\n    - " + tc.pattern + "\n"
			_, err := LoadConfigFromBytes("test", []byte(yaml))
			if err == nil || !strings.Contains(err.Error(), "guard.extra_patterns") || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want a guard.extra_patterns error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// Duplicate extra_patterns names must fail at load: the scanner constructor
// rejects them, so without this check startup would silently drop ALL custom
// rules (degrade path) while reload of the same file fails outright.
func TestGuardExtraPatternsDuplicateName(t *testing.T) {
	yaml := guardTestBaseYAML + `guard:
  extra_patterns:
    - {name: myvendor_key, regex: 'mv-[0-9]{32}'}
    - {name: other_token, regex: 'ot-[0-9a-f]{16}'}
    - {name: myvendor_key, regex: 'mv2-[0-9]{32}'}
`
	_, err := LoadConfigFromBytes("test", []byte(yaml))
	if err == nil {
		t.Fatal("duplicate extra_patterns name accepted, want a load error")
	}
	if !strings.Contains(err.Error(), "guard.extra_patterns[2]") || !strings.Contains(err.Error(), `"myvendor_key"`) || !strings.Contains(err.Error(), "duplicates guard.extra_patterns[0]") {
		t.Errorf("err = %v, want the duplicate name and both indexes", err)
	}
}

// An infix literal flanked by non-literal regex parts on BOTH sides is legal:
// the padded candidates must cover fill+literal+fill (e.g. "mv-" inside
// `[0-9]mv-[0-9]`), and token characters "." / ":" must work as fill.
func TestGuardExtraPatternsInfixLiteral(t *testing.T) {
	yaml := guardTestBaseYAML + `guard:
  extra_patterns:
    - {name: mv_pair, regex: '[0-9]mv-[0-9]', literal: 'mv-'}
    - {name: dotted_key, regex: 'dk\.[a-z]{8}', literal: 'dk.'}
    - {name: scheme_token, regex: 'st:[a-z]{8}', literal: 'st:'}
`
	cfg, err := LoadConfigFromBytes("test", []byte(yaml))
	if err != nil {
		t.Fatalf("infix/dotted/colon literals must validate: %v", err)
	}
	if len(cfg.Guard.ExtraPatterns) != 3 {
		t.Fatalf("ExtraPatterns len = %d, want 3", len(cfg.Guard.ExtraPatterns))
	}
}

func TestGuardExtraPatternsValid(t *testing.T) {
	yaml := guardTestBaseYAML + `guard:
  extra_patterns:
    - {name: myvendor_key, regex: '\bmv-[A-Za-z0-9]{32,}', literal: 'mv-'}
    - {name: other_token, regex: 'ot-[0-9a-f]{16}'}
`
	cfg, err := LoadConfigFromBytes("test", []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Guard.ExtraPatterns) != 2 {
		t.Fatalf("ExtraPatterns len = %d, want 2", len(cfg.Guard.ExtraPatterns))
	}
	p := cfg.Guard.ExtraPatterns[0]
	if p.Name != "myvendor_key" || p.Regex != `\bmv-[A-Za-z0-9]{32,}` || p.Literal != "mv-" {
		t.Errorf("ExtraPatterns[0] = %+v, want the declared values", p)
	}
	if q := cfg.Guard.ExtraPatterns[1]; q.Name != "other_token" || q.Literal != "" {
		t.Errorf("ExtraPatterns[1] = %+v, want name other_token with empty literal", q)
	}
}

func TestGuardExtraPaths(t *testing.T) {
	// Blank entry is a config error, not silently dropped.
	_, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+
		"guard:\n  extra_paths:\n    - ~/.company/secrets\n    - '   '\n"))
	if err == nil || !strings.Contains(err.Error(), "guard.extra_paths") {
		t.Errorf("blank extra_paths entry: err = %v, want a guard.extra_paths error", err)
	}
	// Values load verbatim (~ kept literal) and exact duplicates are deduped.
	cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+
		"guard:\n  extra_paths:\n    - ~/.company/secrets\n    - /etc/shadow\n    - ~/.company/secrets\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"~/.company/secrets", "/etc/shadow"}
	if len(cfg.Guard.ExtraPaths) != len(want) {
		t.Fatalf("ExtraPaths = %v, want %v (deduped)", cfg.Guard.ExtraPaths, want)
	}
	for i := range want {
		if cfg.Guard.ExtraPaths[i] != want[i] {
			t.Errorf("ExtraPaths[%d] = %q, want %q", i, cfg.Guard.ExtraPaths[i], want[i])
		}
	}
}
