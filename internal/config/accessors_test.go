package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStatsConfigAccessors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := (StatsConfig{DBPath: "/tmp/x.db"}).ResolvedDBPath(); got != "/tmp/x.db" {
		t.Errorf("explicit db path = %q, want /tmp/x.db", got)
	}
	if got := (StatsConfig{DBPath: "~/stats.db"}).ResolvedDBPath(); got != filepath.Join(home, "stats.db") {
		t.Errorf("expanded db path = %q, want %q", got, filepath.Join(home, "stats.db"))
	}
	if got := (StatsConfig{}).ResolvedDBPath(); got != filepath.Join(home, ".model-proxy", "stats.db") {
		t.Errorf("default db path = %q", got)
	}

	for _, tc := range []struct {
		name string
		cfg  StatsConfig
		want time.Duration
	}{
		{name: "default", want: 720 * time.Hour},
		{name: "zero", cfg: StatsConfig{Retention: "0"}, want: 0},
		{name: "valid", cfg: StatsConfig{Retention: "48h"}, want: 48 * time.Hour},
		{name: "invalid", cfg: StatsConfig{Retention: "bad"}, want: 720 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.RetentionDuration(); got != tc.want {
				t.Errorf("RetentionDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPricingConfigAccessors(t *testing.T) {
	if (PricingConfig{}).IsEnabled() {
		t.Error("zero-value pricing config should be disabled")
	}
	if !(PricingConfig{Enabled: true}).IsEnabled() {
		t.Error("enabled pricing config should report enabled")
	}
	for _, tc := range []struct {
		name string
		cfg  PricingConfig
		want time.Duration
	}{
		{name: "default", want: pricingDefaultTTLForTest},
		{name: "valid", cfg: PricingConfig{TTL: "2h"}, want: 2 * time.Hour},
		{name: "invalid", cfg: PricingConfig{TTL: "bad"}, want: pricingDefaultTTLForTest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.TTLDuration(); got != tc.want {
				t.Errorf("TTLDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}

const pricingDefaultTTLForTest = 24 * time.Hour

func TestRequestLogConfigAccessors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	def := RequestLogConfig{}
	if got := def.ResolvedDir(); got != filepath.Join(home, ".model-proxy", "log", "requests") {
		t.Errorf("default dir = %q", got)
	}
	if got := (RequestLogConfig{Dir: "/tmp/requests"}).ResolvedDir(); got != "/tmp/requests" {
		t.Errorf("explicit dir = %q", got)
	}
	if got := def.ResolvedMCPDir(); got != filepath.Join(home, ".model-proxy", "log", "mcp") {
		t.Errorf("default mcp dir = %q", got)
	}
	if got := (RequestLogConfig{MCPDir: "/tmp/mcp"}).ResolvedMCPDir(); got != "/tmp/mcp" {
		t.Errorf("explicit mcp dir = %q", got)
	}
	if got := (RequestLogConfig{MCPDir: "~/mcplogs"}).ResolvedMCPDir(); got != filepath.Join(home, "mcplogs") {
		t.Errorf("expanded mcp dir = %q", got)
	}
	if got := def.MaxFileSizeBytes(); got != 1<<30 {
		t.Errorf("default max file size = %d", got)
	}
	if got := (RequestLogConfig{MaxFileSize: 2048}).MaxFileSizeBytes(); got != 2048 {
		t.Errorf("explicit max file size = %d", got)
	}
	if got := (RequestLogConfig{MaxFileSize: -1}).MaxFileSizeBytes(); got != 1<<30 {
		t.Errorf("negative max file size fallback = %d", got)
	}
	if got := def.MaxBodyBytesValue(); got != 5*1024*1024 {
		t.Errorf("default max body = %d", got)
	}
	if got := (RequestLogConfig{MaxBodyBytes: 1024}).MaxBodyBytesValue(); got != 1024 {
		t.Errorf("explicit max body = %d", got)
	}
	if got := (RequestLogConfig{MaxBodyBytes: -1}).MaxBodyBytesValue(); got != 5*1024*1024 {
		t.Errorf("negative max body fallback = %d", got)
	}

	for _, tc := range []struct {
		name string
		cfg  RequestLogConfig
		want time.Duration
	}{
		{name: "default", want: 720 * time.Hour},
		{name: "zero", cfg: RequestLogConfig{Retention: "0"}, want: 0},
		{name: "valid", cfg: RequestLogConfig{Retention: "12h"}, want: 12 * time.Hour},
		{name: "invalid", cfg: RequestLogConfig{Retention: "bad"}, want: 720 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.RetentionDuration(); got != tc.want {
				t.Errorf("RetentionDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequestLogSessionHeaders(t *testing.T) {
	if got := (RequestLogConfig{}).ResolvedSessionHeaders(); !reflect.DeepEqual(got, DefaultSessionHeaders) {
		t.Errorf("default session headers = %v, want %v", got, DefaultSessionHeaders)
	}
	custom := []string{"x-my-session"}
	if got := (RequestLogConfig{SessionHeaders: custom}).ResolvedSessionHeaders(); !reflect.DeepEqual(got, custom) {
		t.Errorf("configured session headers = %v, want %v", got, custom)
	}
	// The default allowlist must not contain per-request ids.
	for _, h := range DefaultSessionHeaders {
		if h == "x-client-request-id" {
			t.Error("x-client-request-id is per-request on most clients and must not be a default session header")
		}
	}
}

func TestCacheConfigAccessors(t *testing.T) {
	def := CacheConfig{}
	if def.IsEnabled() {
		t.Error("zero-value cache config should be disabled")
	}
	if !(CacheConfig{Enabled: true}).IsEnabled() {
		t.Error("enabled cache config should report enabled")
	}
	if got := def.TTLDuration(); got != 10*time.Minute {
		t.Errorf("default ttl = %v", got)
	}
	if got := (CacheConfig{TTL: "2m"}).TTLDuration(); got != 2*time.Minute {
		t.Errorf("explicit ttl = %v", got)
	}
	if got := (CacheConfig{TTL: "bad"}).TTLDuration(); got != 10*time.Minute {
		t.Errorf("invalid ttl fallback = %v", got)
	}
	if got := def.MaxEntriesValue(); got != 1000 {
		t.Errorf("default max entries = %d", got)
	}
	if got := (CacheConfig{MaxEntries: 25}).MaxEntriesValue(); got != 25 {
		t.Errorf("explicit max entries = %d", got)
	}
	if got := (CacheConfig{MaxEntries: -1}).MaxEntriesValue(); got != 1000 {
		t.Errorf("negative max entries fallback = %d", got)
	}
	if got := def.MaxBodyBytesValue(); got != 256*1024 {
		t.Errorf("default max body = %d", got)
	}
	if got := (CacheConfig{MaxBodyBytes: 512}).MaxBodyBytesValue(); got != 512 {
		t.Errorf("explicit max body = %d", got)
	}
	if got := (CacheConfig{MaxBodyBytes: -1}).MaxBodyBytesValue(); got != 256*1024 {
		t.Errorf("negative max body fallback = %d", got)
	}
}

func TestParseHHMM(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{in: "00:00", want: 0, ok: true},
		{in: " 23:59 ", want: 23*60 + 59, ok: true},
		{in: "24:00"},
		{in: "12:60"},
		{in: "12"},
		{in: ""},
		{in: "ab:cd"},
	} {
		got, ok := parseHHMM(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseHHMM(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseHHMMRange(t *testing.T) {
	start, end, ok := parseHHMMRange("09:00-18:00")
	if !ok || start != 9*60 || end != 18*60 {
		t.Errorf("valid range = (%d, %d, %v)", start, end, ok)
	}
	for _, value := range []string{"bad", "09:00-18:00-20:00", "bad-18:00", "09:00-bad"} {
		if _, _, ok := parseHHMMRange(value); ok {
			t.Errorf("parseHHMMRange(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLoadConfigReadError(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("LoadConfig missing file error = %v", err)
	}
}
