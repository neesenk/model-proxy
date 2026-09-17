package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- Scheduling default + invalid-duration fallbacks ---

// YAML-load coverage (pitfalls #13): yaml.v3 silently ignores unknown keys, so
// a mistyped `stream_keepalive` tag would pass every struct-construction test
// above while never landing from a real config file.
func TestScheduling_StreamKeepaliveYAMLLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	base := "listen: 127.0.0.1:0\nproviders:\n  p:\n    provider_id: zhipu\n    openai_base_url: https://x\n"
	cfg, err := LoadConfigFromBytes(path, []byte(base+"scheduling:\n  stream_keepalive: 30s\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	if cfg.Scheduling.Keepalive() != 30*time.Second {
		t.Fatalf("keepalive from YAML = %v, want 30s", cfg.Scheduling.Keepalive())
	}
	if _, err := LoadConfigFromBytes(path, []byte(base+"scheduling:\n  stream_keepalive: -1s\n")); err == nil {
		t.Fatal("negative stream_keepalive accepted, want validation error")
	}
}

func TestScheduling_DefaultsAndInvalid(t *testing.T) {
	var s Scheduling // all zero
	if s.Threshold() != 3 {
		t.Errorf("default threshold=%d want 3", s.Threshold())
	}
	if s.Cooldown() != 10*time.Minute {
		t.Errorf("default cooldown=%v want 10m", s.Cooldown())
	}
	if s.RateBackoff() != 60*time.Second {
		t.Errorf("default rateBackoff=%v want 60s", s.RateBackoff())
	}
	if s.QuotaCooldownDuration() != time.Hour {
		t.Errorf("default quotaCooldown=%v want 1h", s.QuotaCooldownDuration())
	}
	if s.ModelLockoutDuration() != 10*time.Minute {
		t.Errorf("default modelLockout=%v want 10m", s.ModelLockoutDuration())
	}
	if s.RetryWaitDuration() != 10*time.Second {
		t.Errorf("default retryWait=%v want 10s", s.RetryWaitDuration())
	}
	if s.Timeout() != 1800*time.Second {
		t.Errorf("default timeout=%v want 1800s", s.Timeout())
	}
	if s.Keepalive() != 15*time.Second {
		t.Errorf("default keepalive=%v want 15s", s.Keepalive())
	}
	if s.Dwell() != 10*time.Minute {
		t.Errorf("default dwell=%v want 10m", s.Dwell())
	}
	if s.PollInterval() != 5*time.Minute {
		t.Errorf("default pollInterval=%v want 5m", s.PollInterval())
	}
	if s.SwitchMargin() != 0.15 {
		t.Errorf("default switchMargin=%v want 0.15", s.SwitchMargin())
	}
	if s.QualityErrorWeightValue() != 1.0 {
		t.Errorf("default qualityErrorWeight=%v want 1.0", s.QualityErrorWeightValue())
	}
	if s.QualityTTFTWeightValue() != 0.2 {
		t.Errorf("default qualityTTFTWeight=%v want 0.2", s.QualityTTFTWeightValue())
	}

	// Explicit 0 disables the signal (pointer semantics); explicit values win.
	zero, forty, five := 0, 40, 5
	custom := Scheduling{QualityErrorWeight: &forty, QualityTTFTWeight: &five}
	if custom.QualityErrorWeightValue() != 0.4 || custom.QualityTTFTWeightValue() != 0.05 {
		t.Errorf("explicit weights = %v/%v, want 0.4/0.05",
			custom.QualityErrorWeightValue(), custom.QualityTTFTWeightValue())
	}
	disabled := Scheduling{QualityErrorWeight: &zero, QualityTTFTWeight: &zero}
	if disabled.QualityErrorWeightValue() != 0 || disabled.QualityTTFTWeightValue() != 0 {
		t.Errorf("explicit 0 must disable: %v/%v",
			disabled.QualityErrorWeightValue(), disabled.QualityTTFTWeightValue())
	}

	// Invalid duration strings fall back to defaults.
	bad := Scheduling{
		CircuitCooldown:   "bad",
		RateLimitBackoff:  "bad",
		QuotaCooldown:     "bad",
		ModelLockout:      "bad",
		RetryWait:         "bad",
		UpstreamTimeout:   "bad",
		StreamKeepalive:   "bad",
		StickyDwell:       "bad",
		QuotaPollInterval: "bad",
	}
	if bad.Cooldown() != 10*time.Minute {
		t.Errorf("invalid cooldown fallback=%v want 10m", bad.Cooldown())
	}
	if bad.RateBackoff() != 60*time.Second {
		t.Errorf("invalid rateBackoff fallback=%v want 60s", bad.RateBackoff())
	}
	if bad.QuotaCooldownDuration() != time.Hour {
		t.Errorf("invalid quotaCooldown fallback=%v want 1h", bad.QuotaCooldownDuration())
	}
	if bad.ModelLockoutDuration() != 10*time.Minute {
		t.Errorf("invalid modelLockout fallback=%v want 10m", bad.ModelLockoutDuration())
	}
	if bad.RetryWaitDuration() != 10*time.Second {
		t.Errorf("invalid retryWait fallback=%v want 10s", bad.RetryWaitDuration())
	}
	if bad.Timeout() != 1800*time.Second {
		t.Errorf("invalid timeout fallback=%v want 1800s", bad.Timeout())
	}
	if bad.Keepalive() != 15*time.Second {
		t.Errorf("invalid keepalive fallback=%v want 15s", bad.Keepalive())
	}
	if bad.Dwell() != 10*time.Minute {
		t.Errorf("invalid dwell fallback=%v want 10m", bad.Dwell())
	}
	if bad.PollInterval() != 5*time.Minute {
		t.Errorf("invalid pollInterval fallback=%v want 5m", bad.PollInterval())
	}

	// Explicit values are honored.
	good := Scheduling{
		CircuitThreshold:  5,
		CircuitCooldown:   "2m",
		RateLimitBackoff:  "10s",
		QuotaCooldown:     "2h",
		ModelLockout:      "30m",
		RetryWait:         "0",
		UpstreamTimeout:   "5s",
		StreamKeepalive:   "30s",
		StickyDwell:       "3m",
		QuotaPollInterval: "1m",
		QuotaSwitchMargin: 25,
	}
	if good.Threshold() != 5 {
		t.Errorf("explicit threshold=%d want 5", good.Threshold())
	}
	if good.Cooldown() != 2*time.Minute {
		t.Errorf("explicit cooldown=%v want 2m", good.Cooldown())
	}
	if good.RateBackoff() != 10*time.Second {
		t.Errorf("explicit rateBackoff=%v want 10s", good.RateBackoff())
	}
	if good.QuotaCooldownDuration() != 2*time.Hour {
		t.Errorf("explicit quotaCooldown=%v want 2h", good.QuotaCooldownDuration())
	}
	if good.ModelLockoutDuration() != 30*time.Minute {
		t.Errorf("explicit modelLockout=%v want 30m", good.ModelLockoutDuration())
	}
	if good.RetryWaitDuration() != 0 {
		t.Errorf("explicit retryWait=%v want 0", good.RetryWaitDuration())
	}
	if good.Timeout() != 5*time.Second {
		t.Errorf("explicit timeout=%v want 5s", good.Timeout())
	}
	if good.Keepalive() != 30*time.Second {
		t.Errorf("explicit keepalive=%v want 30s", good.Keepalive())
	}
	if (Scheduling{StreamKeepalive: "0"}).Keepalive() != 0 {
		t.Errorf("keepalive \"0\" must disable, got %v", (Scheduling{StreamKeepalive: "0"}).Keepalive())
	}
	if good.Dwell() != 3*time.Minute {
		t.Errorf("explicit dwell=%v want 3m", good.Dwell())
	}
	if good.PollInterval() != time.Minute {
		t.Errorf("explicit pollInterval=%v want 1m", good.PollInterval())
	}
	if good.SwitchMargin() != 0.25 {
		t.Errorf("explicit switchMargin=%v want 0.25", good.SwitchMargin())
	}
}

// Regression (pitfalls #14): zero/negative durations used to pass validation
// and flow into the runtime — upstream_timeout: "0s" disabled the ONLY
// timeout bound on upstream and shadow requests. Only the fields whose
// contract documents "0" (retry_wait, stats/request_log retention) may be
// zero; everything else must parse and be positive.
func TestValidate_RejectsNonPositiveDurations(t *testing.T) {
	base := func() *Config {
		return &Config{
			Listen:    "127.0.0.1:8080",
			Providers: map[string]Provider{"p": {Provider: "zhipu", OpenAIBaseURL: "https://x"}},
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"scheduling.upstream_timeout", "0s"},
		{"scheduling.upstream_timeout", "-10m"},
		{"scheduling.circuit_cooldown", "0s"},
		{"scheduling.rate_limit_backoff", "-1s"},
		{"scheduling.quota_cooldown", "0h"},
		{"scheduling.model_lockout", "-5m"},
		{"scheduling.sticky_dwell", "0s"},
		{"scheduling.quota_poll_interval", "0s"},
		{"cache.ttl", "0s"},
		{"pricing.ttl", "-1h"},
		{"stats.retention", "-720h"},
		{"request_log.retention", "-1h"},
		{"scheduling.circuit_cooldown", "not-a-duration"},
	} {
		cfg := base()
		switch {
		case strings.HasPrefix(field.name, "scheduling."):
			key := strings.TrimPrefix(field.name, "scheduling.")
			switch key {
			case "upstream_timeout":
				cfg.Scheduling.UpstreamTimeout = field.value
			case "circuit_cooldown":
				cfg.Scheduling.CircuitCooldown = field.value
			case "rate_limit_backoff":
				cfg.Scheduling.RateLimitBackoff = field.value
			case "quota_cooldown":
				cfg.Scheduling.QuotaCooldown = field.value
			case "model_lockout":
				cfg.Scheduling.ModelLockout = field.value
			case "sticky_dwell":
				cfg.Scheduling.StickyDwell = field.value
			case "quota_poll_interval":
				cfg.Scheduling.QuotaPollInterval = field.value
			}
		case field.name == "cache.ttl":
			cfg.Cache.TTL = field.value
		case field.name == "pricing.ttl":
			cfg.Pricing.TTL = field.value
		case field.name == "stats.retention":
			cfg.Stats.Retention = field.value
		case field.name == "request_log.retention":
			cfg.RequestLog.Retention = field.value
		}
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), field.name) {
			t.Errorf("%s=%q: want validation error naming the field, got %v", field.name, field.value, err)
		}
	}

	// The documented zeros stay legal.
	for _, set := range []func(*Config){
		func(c *Config) { c.Scheduling.RetryWait = "0" },
		func(c *Config) { c.Scheduling.StreamKeepalive = "0" },
		func(c *Config) { c.Stats.Retention = "0" },
		func(c *Config) { c.RequestLog.Retention = "0" },
		func(c *Config) { c.Scheduling.UpstreamTimeout = "30m" },
	} {
		cfg := base()
		set(cfg)
		if err := cfg.validate(); err != nil {
			t.Errorf("legal duration rejected: %v", err)
		}
	}
}
