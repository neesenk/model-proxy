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
}

// Scheduling configures failover health (circuit breaker, rate-limit skip) and
// sticky routing. Durations are parsed from strings (e.g. "10m", "60s") via
// time.ParseDuration; unset/invalid values fall back to the defaults shown below.
type Scheduling struct {
	CircuitThreshold int    `yaml:"circuit_threshold"`  // consecutive failures → open circuit (default 3)
	CircuitCooldown  string `yaml:"circuit_cooldown"`   // circuit open duration, then half-open 1 probe (default 10m)
	RateLimitBackoff string `yaml:"rate_limit_backoff"` // 429 with no Retry-After: skip this long, then probe (default 60s)
	UpstreamTimeout  string `yaml:"upstream_timeout"`   // per-upstream-request timeout (default 30s)
	StickyDwell      string `yaml:"sticky_dwell"`       // min time on the chosen provider before re-evaluating (default 10m)
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

type Provider struct {
	// OpenAIBaseURL is the default upstream base — used for the OpenAI protocol
	// (/chat/completions, /responses) plus /models and usage. AnthropicBaseURL
	// optionally overrides it for anthropic (/v1/messages) requests; if unset,
	// OpenAIBaseURL serves both protocols. Both must include their version segment
	// (e.g. .../v1, .../anthropic/v1) since the proxy strips the client's /v1.
	OpenAIBaseURL    string                   `yaml:"openai_base_url"`
	AnthropicBaseURL string                   `yaml:"anthropic_base_url"`
	Provider         string                   `yaml:"provider_id"`
	CQPMintURL       string                   `yaml:"cqp_mint_url"` // compass only
	Headers          map[string]string        `yaml:"headers"`
	UsageURL         string                   `yaml:"usage_url"`
	Models           map[string]ProviderModel `yaml:"models"`
	// PeakHours is this provider's peak window "HH:MM-HH:MM" (local time). Route
	// scheduling tries non-peak providers first (by priority), then peak ones —
	// so a provider in its peak window is deprioritized (高峰期扣减更多 → 少用).
	PeakHours string `yaml:"peak_hours"`
}

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
// (non-peak providers first, then by priority) and fails over to the next on error.
type RouteTarget struct {
	Provider string `yaml:"provider"` // config providers[] key
	Model    string `yaml:"model"`    // real model name at that provider
	Priority int    `yaml:"priority"` // lower = tried first within a peak group (default 0)
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
	}
	raw := rawConfig{
		Listen:   "127.0.0.1:15721",
		LogLevel: "info",
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
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

	// Expand log path.
	cfg.LogFile = expandPath(cfg.LogFile)
	// Expand takeover paths.
	t := &cfg.Takeover
	t.Claude = expandPath(t.Claude)
	t.Opencode = expandPath(t.Opencode)
	t.Codex = expandPath(t.Codex)
	t.Pi = expandPath(t.Pi)
	// If proxy_url is unset, derive it from `listen`.
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
			return fmt.Errorf("provider %q: provider_id is empty — set `provider_id:` (e.g. zhipu, compass, codex, deepseek, volcengine)", name)
		}
		// Check for known provider_id typos.
		known := map[string]bool{"compass": true, "codex": true, "zhipu": true, "deepseek": true, "volcengine": true, "apikey": true, "static": true}
		if !known[p.Provider] {
			return fmt.Errorf("provider %q: unknown provider_id %q — valid: compass, codex, zhipu, deepseek, volcengine", name, p.Provider)
		}
		// anthropic_base_url should NOT end with /v1 (proxy keeps client's /v1 for anthropic).
		if p.AnthropicBaseURL != "" && (strings.HasSuffix(p.AnthropicBaseURL, "/v1") || strings.HasSuffix(p.AnthropicBaseURL, "/v1/")) {
			return fmt.Errorf("provider %q: anthropic_base_url ends with /v1 — the proxy keeps the client's /v1/messages path; remove the trailing /v1", name)
		}
		// usage_url should use https.
		if p.UsageURL != "" && !strings.HasPrefix(p.UsageURL, "https://") && !strings.HasPrefix(p.UsageURL, "http://") {
			return fmt.Errorf("provider %q: usage_url %q is not a valid URL", name, p.UsageURL)
		}
		// peak_hours should be a valid "HH:MM-HH:MM" window with start != end.
		if p.PeakHours != "" {
			start, end, ok := parseHHMMRange(p.PeakHours)
			if !ok {
				return fmt.Errorf("provider %q: peak_hours %q is malformed — expected \"HH:MM-HH:MM\"", name, p.PeakHours)
			}
			if start == end {
				return fmt.Errorf("provider %q: peak_hours %q has zero-width window (start == end)", name, p.PeakHours)
			}
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
	// Check for duplicate priorities within the same route (warn but don't fail —
	// ties are resolved by list order, but duplicate priorities are likely a mistake).
	for exposed, targets := range c.Routes {
		seen := map[int]bool{}
		for _, t := range targets {
			if seen[t.Priority] && t.Priority != 0 {
				return fmt.Errorf("route %q: duplicate priority %d — targets with the same priority are ambiguous; use distinct priorities", exposed, t.Priority)
			}
			seen[t.Priority] = true
		}
	}
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
