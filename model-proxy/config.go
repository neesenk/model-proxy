package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen        string                   `yaml:"listen"`
	LogLevel      string                   `yaml:"log_level"`
	LogFile       string                   `yaml:"log_file"`
	Providers     map[string]Provider      `yaml:"providers"`
	Routes        map[string][]RouteTarget `yaml:"routes"`
	ClaudeMapping map[string]string        `yaml:"claude_mapping"`
	Scheduling    Scheduling               `yaml:"scheduling"`
	Takeover      Takeover                 `yaml:"takeover"`
	Web           WebConfig                `yaml:"web"`
	Stats         StatsConfig              `yaml:"stats"`
	RequestLog    RequestLogConfig         `yaml:"request_log"`
}

// WebConfig toggles the admin UI (/ui + /api). Defaults to enabled.
type WebConfig struct {
	Enabled bool `yaml:"enabled"`
}

// StatsConfig configures SQLite-backed call-statistics persistence (per
// provider x model x minute buckets). Defaults: db_path ~/.model-proxy/stats.db,
// retention 720h (30 days); retention 0 keeps history forever.
type StatsConfig struct {
	DBPath    string `yaml:"db_path"`
	Retention string `yaml:"retention"`
}

// dbPath returns the SQLite stats DB path, defaulting to ~/.model-proxy/stats.db.
func (s StatsConfig) dbPath() string {
	if s.DBPath != "" {
		return expandPath(s.DBPath)
	}
	return filepath.Join(homeDir(), ".model-proxy", "stats.db")
}

// retention returns the bucket retention duration, defaulting to 30 days.
// 0 or "0" keeps history forever.
func (s StatsConfig) retention() time.Duration {
	if s.Retention == "" {
		return 720 * time.Hour
	}
	if d, err := time.ParseDuration(s.Retention); err == nil {
		return d
	}
	return 720 * time.Hour
}

// RequestLogConfig configures per-request access logging: the full request +
// response bodies of each committed upstream call are written as JSONL lines to
// a rotating file under dir for offline analysis (prompt replay, failure
// debugging, agent behavior). Defaults to DISABLED (zero hot-path overhead).
// Changing enabled requires a restart (reload does not rebuild the logger).
//
// Rotation: a file is closed (and renamed requests-<start>--<end>.log) when the
// next line would exceed max_file_size (default 1 GiB) OR the calendar day
// changes since the file was opened - so even a quiet day yields at most one
// file per day. max_body_bytes (default 5 MiB) caps each captured body, marking
// truncation past it to bound memory. retention (default 720h = 30 days; "0" =
// keep forever) controls a periodic sweep that deletes rotated files older
// than the window; the active file is never deleted.
type RequestLogConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Dir          string `yaml:"dir"`
	MaxFileSize  int64  `yaml:"max_file_size"`
	MaxBodyBytes int    `yaml:"max_body_bytes"`
	Retention    string `yaml:"retention"`
}

// dir returns the request-log directory, defaulting to
// ~/.model-proxy/requests.
func (r RequestLogConfig) dir() string {
	if r.Dir != "" {
		return expandPath(r.Dir)
	}
	return filepath.Join(homeDir(), ".model-proxy", "requests")
}

// maxFileSize returns the per-file rotation cap in bytes, defaulting to 1 GiB.
// Values <= 0 fall back to the default.
func (r RequestLogConfig) maxFileSize() int64 {
	if r.MaxFileSize > 0 {
		return r.MaxFileSize
	}
	return 1 << 30 // 1 GiB
}

// maxBodyBytes returns the per-body capture cap in bytes, defaulting to 5 MiB.
// Values <= 0 fall back to the default.
func (r RequestLogConfig) maxBodyBytes() int {
	if r.MaxBodyBytes > 0 {
		return r.MaxBodyBytes
	}
	return 5 * 1024 * 1024
}

// retention returns the rotated-file retention duration, defaulting to 30 days.
// "0" or a zero duration means keep forever (no sweep).
func (r RequestLogConfig) retention() time.Duration {
	if r.Retention == "" {
		return 720 * time.Hour
	}
	if d, err := time.ParseDuration(r.Retention); err == nil {
		return d
	}
	return 720 * time.Hour
}

// Scheduling configures failover health (circuit breaker, rate-limit skip) and
// sticky routing. Durations are parsed from strings (e.g. "10m", "60s") via
// time.ParseDuration; unset/invalid values fall back to the defaults shown below.
type Scheduling struct {
	CircuitThreshold  int    `yaml:"circuit_threshold"`   // consecutive failures → open circuit (default 3)
	CircuitCooldown   string `yaml:"circuit_cooldown"`    // circuit open duration, then half-open 1 probe (default 10m)
	RateLimitBackoff  string `yaml:"rate_limit_backoff"`  // 429 with no Retry-After: skip this long, then probe (default 60s)
	UpstreamTimeout   string `yaml:"upstream_timeout"`    // per-upstream-request timeout (default 30s)
	StickyDwell       string `yaml:"sticky_dwell"`        // min time on the chosen provider before re-evaluating (default 10m)
	QuotaPollInterval string `yaml:"quota_poll_interval"` // background poll cadence (default 5m)
	QuotaSwitchMargin int    `yaml:"quota_switch_margin"` // switch if another plan provider's effective remaining beats current by ≥ this many pct points (default 15)
}

func (s Scheduling) threshold() int {
	if s.CircuitThreshold > 0 {
		return s.CircuitThreshold
	}
	return 3
}
func (s Scheduling) cooldown() time.Duration {
	if d, err := time.ParseDuration(s.CircuitCooldown); err == nil {
		return d
	}
	return 10 * time.Minute
}
func (s Scheduling) rateBackoff() time.Duration {
	if d, err := time.ParseDuration(s.RateLimitBackoff); err == nil {
		return d
	}
	return 60 * time.Second
}
func (s Scheduling) timeout() time.Duration {
	if d, err := time.ParseDuration(s.UpstreamTimeout); err == nil {
		return d
	}
	return 30 * time.Second
}
func (s Scheduling) dwell() time.Duration {
	if d, err := time.ParseDuration(s.StickyDwell); err == nil {
		return d
	}
	return 10 * time.Minute
}
func (s Scheduling) pollInterval() time.Duration {
	if d, err := time.ParseDuration(s.QuotaPollInterval); err == nil {
		return d
	}
	return 5 * time.Minute
}
func (s Scheduling) switchMargin() float64 {
	if s.QuotaSwitchMargin > 0 {
		return float64(s.QuotaSwitchMargin) / 100.0
	}
	return 0.15
}

type Provider struct {
	// OpenAIBaseURL is the default upstream base — used for the OpenAI protocol
	// (/chat/completions, /responses) plus /models and usage. AnthropicBaseURL
	// optionally overrides it for anthropic (/v1/messages) requests; if unset,
	// OpenAIBaseURL serves both protocols. Both must include their version segment
	// (e.g. .../v1, .../anthropic/v1) since the proxy strips the client's /v1.
	OpenAIBaseURL    string            `yaml:"openai_base_url"`
	AnthropicBaseURL string            `yaml:"anthropic_base_url"`
	Provider         string            `yaml:"provider_id"`
	AqpMintURL       string            `yaml:"aqp_mint_url"` // aqp only
	ClientVersion    string            `yaml:"client_version,omitempty"`
	Headers          map[string]string `yaml:"headers"`
	UsageURL         string            `yaml:"usage_url"`
	// Models is the list of real model names this provider serves — a managed
	// whitelist (kept in sync by `models refresh`). Metadata (context/output/
	// modalities) is NOT stored here; it is sourced at runtime from models.dev
	// (or conservative defaults) by hydrateModels.
	Models []string `yaml:"models"`
	// PeakHours is this provider's set of peak segments (each "HH:MM-HH:MM" in
	// local time). Peak is folded into effective remaining quota: the per-segment
	// multiplier discounts a provider's remaining quota while it is inside a peak
	// window, so a peak provider is deprioritized (scheduled later within its
	// tier/quota band) rather than tried in a separate group.
	PeakHours PeakConfig `yaml:"peak_hours"`
	// Billing is "plan" (default, quota-bound) or "pay-as-you-go" (strict
	// last-resort: used only when all plan providers are unavailable).
	Billing string `yaml:"billing"`
}

// PeakSegment is one peak-hours window with its consumption multiplier.
type PeakSegment struct {
	Window     string  `yaml:"window"`
	Multiplier float64 `yaml:"multiplier"` // 0 → default (2.0) at validate time
}

// PeakConfig is a provider's set of peak segments. It unmarshals from three
// YAML shapes: a single string ("09:00-18:00"), a list of strings, or a list
// of {window, multiplier} maps.
type PeakConfig []PeakSegment

func (p *PeakConfig) UnmarshalYAML(value *yaml.Node) error {
	// Case 1: single string.
	var single string
	if value.Decode(&single) == nil && single != "" {
		*p = PeakConfig{{Window: single}}
		return nil
	}
	// Case 2/3: a sequence.
	var seq []yaml.Node
	if err := value.Decode(&seq); err != nil {
		return err
	}
	out := make(PeakConfig, 0, len(seq))
	for _, el := range seq {
		// Only accept string scalars ("09:00-18:00") or mapping nodes
		// ({window, multiplier}). Reject integers/floats/bools — yaml.v3
		// silently coerces int→string, which would create bogus windows.
		if el.Kind == yaml.ScalarNode && el.Tag != "!!str" && el.Tag != "" {
			return fmt.Errorf("peak_hours element %q: must be a string \"HH:MM-HH:MM\" or a {window, multiplier} map (got %s)", el.Value, el.Tag)
		}
		var s string
		if el.Decode(&s) == nil && s != "" {
			out = append(out, PeakSegment{Window: s})
			continue
		}
		if el.Kind == yaml.MappingNode {
			var seg PeakSegment
			if err := el.Decode(&seg); err != nil {
				return err
			}
			out = append(out, seg)
			continue
		}
		return fmt.Errorf("peak_hours element %q: must be a string \"HH:MM-HH:MM\" or a {window, multiplier} map", el.Value)
	}
	*p = out
	return nil
}

// peakMultiplier returns the multiplier of whichever peak segment `now` falls
// into (1.0 if none / no segments). Used to discount effective remaining quota.
func (p Provider) peakMultiplier(now time.Time) float64 {
	now = now.Local()
	m := now.Hour()*60 + now.Minute()
	for _, seg := range p.PeakHours {
		start, end, ok := parseHHMMRange(seg.Window)
		if !ok {
			continue
		}
		var inside bool
		if start <= end {
			inside = m >= start && m < end
		} else {
			inside = m >= start || m < end // wrap-around
		}
		if inside {
			if seg.Multiplier > 0 {
				return seg.Multiplier
			}
			return defaultPeakMultiplier
		}
	}
	return 1.0
}

const defaultPeakMultiplier = 2.0

type ProviderModel struct {
	Context    int64              `yaml:"context"`
	Output     int                `yaml:"output"`
	Modalities ProviderModalities `yaml:"modalities"`
}

type ProviderModalities struct {
	Input  []string `yaml:"input"`
	Output []string `yaml:"output"`
}

// RouteTarget is one upstream destination for an exposed model name. A route maps
// an exposed name to an ordered list of targets; the proxy picks one by scheduling
// (by tier/quota band, peak folded into effective remaining) and fails over to the
// next on error.
type RouteTarget struct {
	Provider string `yaml:"provider"` // config providers[] key
	Model    string `yaml:"model"`    // real model name at that provider
	Priority int    `yaml:"priority"` // lower = tried first within a tier/quota band (default 0)
}

type Takeover struct {
	ProxyURL string `yaml:"proxy_url"`
	Claude   string `yaml:"claude"`
	Opencode string `yaml:"opencode"`
	Codex    string `yaml:"codex"`
	Pi       string `yaml:"pi"`
	// ProviderID is the single provider identifier used by takeover for every
	// agent that takes one (opencode, pi, codex, and future agents). claude
	// doesn't use it (it writes env vars). Default "model-proxy".
	ProviderID string `yaml:"provider_id"`
}

// expandPath expands ~ and the env: prefix.
func expandPath(p string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "env:") {
		return os.Getenv(p[4:])
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return LoadConfigFromBytes(path, data)
}

// LoadConfigFromBytes parses + validates config bytes (path is used for error
// messages + relative-path resolution only). Shared by LoadConfig (disk) and
// the web layer's validate-before-write (in-memory YAML edit).
func LoadConfigFromBytes(path string, data []byte) (*Config, error) {
	cfg := &Config{}
	type rawConfig struct {
		Listen        string                   `yaml:"listen"`
		LogLevel      string                   `yaml:"log_level"`
		LogFile       string                   `yaml:"log_file"`
		Providers     map[string]Provider      `yaml:"providers"`
		Routes        map[string][]RouteTarget `yaml:"routes"`
		ClaudeMapping map[string]string        `yaml:"claude_mapping"`
		Scheduling    Scheduling               `yaml:"scheduling"`
		Takeover      Takeover                 `yaml:"takeover"`
		Web           WebConfig                `yaml:"web"`
		Stats         StatsConfig              `yaml:"stats"`
		RequestLog    RequestLogConfig         `yaml:"request_log"`
	}
	raw := rawConfig{
		Listen:   "127.0.0.1:15721",
		LogLevel: "info",
		Web:      WebConfig{Enabled: true},
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// The most common breakage: a providers' `models:` block still in the
		// old map form (glm-5.2: {context, output, modalities}) after the switch
		// to a name list. The raw yaml error ("cannot unmarshal !!map into
		// []string") is opaque — add a hint pointing at the fix.
		if strings.Contains(err.Error(), "!!map") && strings.Contains(err.Error(), "[]string") {
			return nil, fmt.Errorf("parse yaml: %w\nhint: a 'models:' block must be a list of model names, e.g.\n  models:\n    - glm-5.2\n    - glm-4.6\nThe old map form (name: {context, output, modalities}) is no longer supported — metadata is now auto-sourced from models.dev at runtime", err)
		}
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	cfg.Listen = raw.Listen
	cfg.LogLevel = raw.LogLevel
	cfg.LogFile = raw.LogFile
	cfg.Providers = raw.Providers
	cfg.Routes = raw.Routes
	cfg.ClaudeMapping = raw.ClaudeMapping
	cfg.Scheduling = raw.Scheduling
	cfg.Takeover = raw.Takeover
	cfg.Web = raw.Web
	cfg.Stats = raw.Stats
	cfg.RequestLog = raw.RequestLog
	cfg.LogFile = expandPath(cfg.LogFile)
	t := &cfg.Takeover
	// Takeover paths default to each client's standard config location (and
	// provider_id to "model-proxy"), so config.yaml can omit the entire
	// `takeover:` block unless overriding one. Set before expandPath so the
	// `~` in the defaults is expanded (same as explicitly-configured paths).
	if t.ProviderID == "" {
		t.ProviderID = "model-proxy"
	}
	if t.Claude == "" {
		t.Claude = "~/.claude/settings.json"
	}
	if t.Opencode == "" {
		t.Opencode = "~/.config/opencode/opencode.json"
	}
	if t.Codex == "" {
		t.Codex = "~/.codex/config.toml"
	}
	if t.Pi == "" {
		t.Pi = "~/.pi/agent/models.json"
	}
	t.Claude = expandPath(t.Claude)
	t.Opencode = expandPath(t.Opencode)
	t.Codex = expandPath(t.Codex)
	t.Pi = expandPath(t.Pi)
	if t.ProxyURL == "" && cfg.Listen != "" {
		t.ProxyURL = "http://" + cfg.Listen
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate returns nil if the config is valid. On failure it returns an error
// with a human-readable message including a hint for fixing the issue.
func (c *Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen is empty — set `listen: 127.0.0.1:PORT` in config")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured — add at least one under `providers:`")
	}
	for name, p := range c.Providers {
		if p.OpenAIBaseURL == "" {
			return fmt.Errorf("provider %q: openai_base_url is empty — set it under providers.%s", name, name)
		}
		if p.Provider == "" {
			return fmt.Errorf("provider %q: provider_id is empty — set `provider_id:` (e.g. zhipu, aqp, codex, deepseek, volcengine)", name)
		}
		// Check for known provider_id typos.
		known := map[string]bool{"aqp": true, "codex": true, "zhipu": true, "deepseek": true, "volcengine": true, "apikey": true, "static": true}
		if !known[p.Provider] {
			return fmt.Errorf("provider %q: unknown provider_id %q — valid: aqp, codex, zhipu, deepseek, volcengine", name, p.Provider)
		}
		// anthropic_base_url should NOT end with /v1 (proxy keeps client's /v1 for anthropic).
		if p.AnthropicBaseURL != "" && (strings.HasSuffix(p.AnthropicBaseURL, "/v1") || strings.HasSuffix(p.AnthropicBaseURL, "/v1/")) {
			return fmt.Errorf("provider %q: anthropic_base_url ends with /v1 — the proxy keeps the client's /v1/messages path; remove the trailing /v1", name)
		}
		// usage_url should use https.
		if p.UsageURL != "" && !strings.HasPrefix(p.UsageURL, "https://") && !strings.HasPrefix(p.UsageURL, "http://") {
			return fmt.Errorf("provider %q: usage_url %q is not a valid URL", name, p.UsageURL)
		}
		// peak_hours: each segment window must be a valid HH:MM-HH:MM range.
		for i, seg := range p.PeakHours {
			start, end, ok := parseHHMMRange(seg.Window)
			if !ok {
				return fmt.Errorf("provider %q: peak_hours segment %d %q is malformed — expected \"HH:MM-HH:MM\"", name, i, seg.Window)
			}
			if start == end {
				return fmt.Errorf("provider %q: peak_hours segment %d %q has zero-width window", name, i, seg.Window)
			}
			if seg.Multiplier < 0 {
				return fmt.Errorf("provider %q: peak_hours segment %d multiplier %v must be > 0", name, i, seg.Multiplier)
			}
		}
		// billing: only known values.
		if p.Billing != "" && p.Billing != "plan" && p.Billing != "pay-as-you-go" {
			return fmt.Errorf("provider %q: billing %q invalid — use \"plan\" or \"pay-as-you-go\"", name, p.Billing)
		}
	}
	for exposed, targets := range c.Routes {
		if len(targets) == 0 {
			return fmt.Errorf("route %q: no targets — add at least one {provider, model}", exposed)
		}
		for i, t := range targets {
			if t.Provider == "" {
				return fmt.Errorf("route %q target %d: provider is empty", exposed, i)
			}
			if t.Model == "" {
				return fmt.Errorf("route %q target %d: model is empty", exposed, i)
			}
			if _, ok := c.Providers[t.Provider]; !ok {
				return fmt.Errorf("route %q target %d: provider %q not defined under providers: — check spelling or add the provider", exposed, i, t.Provider)
			}
		}
	}
	// claude_mapping values must reference a route key.
	for claude, exposed := range c.ClaudeMapping {
		if exposed == "" {
			return fmt.Errorf("claude_mapping %q: target is empty — set it to a route name", claude)
		}
		if _, ok := c.Routes[exposed]; !ok {
			return fmt.Errorf("claude_mapping %q → %q: target %q not found in routes — add a route named %q or fix the mapping", claude, exposed, exposed, exposed)
		}
	}
	// NOTE: duplicate priorities within a route are intentionally allowed. The
	// scheduler (proxy.go decideOrder) ranks by tier -> priority -> surplus, so
	// same-priority targets form a surplus-competed pool (higher-surplus wins;
	// sticky switching only on a margin edge). Do NOT re-add a duplicate-priority
	// rejection here - it contradicts the scheduling contract (AGENTS.md
	// "quota-aware scheduling").
	return nil
}

// protocolForPath returns the protocol name (anthropic|openai) for a request
// path, or "" if no route matches.
func protocolForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return "anthropic"
	case strings.HasPrefix(path, "/v1/chat/completions"),
		strings.HasPrefix(path, "/v1/responses"):
		return "openai"
	}
	return ""
}
