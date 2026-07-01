package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen   string   `yaml:"listen"`
	LogLevel string   `yaml:"log_level"`
	LogFile  string   `yaml:"log_file"`
	ModelsCacheFile        string `yaml:"models_cache_file"`
	ModelsRefreshInterval  string `yaml:"models_refresh_interval"`
	Auth     AuthCfg  `yaml:"auth"`
	Routes   []Route  `yaml:"routes"` // preserve order; yaml maps aren't ordered, so use a struct slice
	Takeover Takeover `yaml:"takeover"`
}

type AuthCfg struct {
	SSOCookieFile string `yaml:"sso_cookie_file"`
	CQPMintURL    string `yaml:"cqp_mint_url"`
	StaticKey     string `yaml:"static_key"`
	// CodexAuthFile is ~/.codex/auth.json — read by the codex_oauth auth provider
	// to get the ChatGPT access/refresh tokens for the codex native backend.
	CodexAuthFile string `yaml:"codex_auth_file"`
}

type Route struct {
	Name          string            `yaml:"name"`
	PathPrefixes  []string          `yaml:"path_prefixes"`
	Upstream      string            `yaml:"upstream"`
	UpstreamPath  string            `yaml:"upstream_path"`
	Auth          string            `yaml:"auth"` // cqp | codex_oauth | static | none
	ModelMap      map[string]string `yaml:"model_map"`
	// ModelRouting enables per-model upstream/auth selection within this route.
	// A request whose model matches an entry's Models uses that entry's
	// Upstream/Auth/ModelMap; otherwise the route's top-level fields are used.
	// Used by the codex route to send codex-native models (gpt-5.5) to the
	// chatgpt.com backend (codex OAuth) and gateway models to compass (CQP).
	ModelRouting []ModelRoute `yaml:"model_routing"`
}

// ModelRoute is one per-model routing entry within a Route.
type ModelRoute struct {
	Models   []string          `yaml:"models"`
	Upstream string            `yaml:"upstream"`
	Auth     string            `yaml:"auth"` // cqp | codex_oauth | static | none
	ModelMap map[string]string `yaml:"model_map"`
}

type Takeover struct {
	ProxyURL     string                `yaml:"proxy_url"`
	ClaudeFile   string                `yaml:"claude_file"`
	OpencodeFile string                `yaml:"opencode_file"`
	CodexFile    string                `yaml:"codex_file"`
	PiFile       string                `yaml:"pi_file"`
	// ProviderID is the single provider identifier used by takeover for every
	// agent that takes one (opencode, pi, codex, and future agents). claude
	// doesn't use it (it writes env vars). Default "ais-switch-proxy".
	ProviderID   string                `yaml:"provider_id"`
	PiModels     []string              `yaml:"pi_models"`
	// ModelLimits holds per-model output limits (max output tokens, modalities).
	// Keyed by model id. Used by takeover when writing client configs. Models not
	// listed fall back to DefaultOutputTokens / default modalities. This keeps
	// model-specific values in config, not hardcoded in client-rewrite code.
	ModelLimits         map[string]ModelLimit `yaml:"model_limits"`
}

// ModelLimit is per-model metadata takeover writes into client configs.
// Context is optional; when 0, the value from the gateway models cache is used
// (and if that's missing too, the field is omitted).
type ModelLimit struct {
	Context      int64    `yaml:"context"`
	OutputTokens int      `yaml:"output_tokens"`
	Input        []string `yaml:"input"`
	Output       []string `yaml:"output"`
}

// defaultModelLimit is the fallback when a model isn't in ModelLimits.
var defaultModelLimit = ModelLimit{
	OutputTokens: 4096,
	Input:        []string{"text"},
	Output:       []string{"text"},
}

// modelLimit returns the limit for a model, applying config overrides on top of
// the default (so partial config still fills the gaps).
func (t *Takeover) modelLimit(modelID string) ModelLimit {
	m := defaultModelLimit
	if ml, ok := t.ModelLimits[modelID]; ok {
		if ml.OutputTokens > 0 {
			m.OutputTokens = ml.OutputTokens
		}
		if len(ml.Input) > 0 {
			m.Input = ml.Input
		}
		if len(ml.Output) > 0 {
			m.Output = ml.Output
		}
		if ml.Context > 0 {
			m.Context = ml.Context
		}
	}
	return m
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
	// Decode into a temp struct first, then process routes.
	type rawConfig struct {
		Listen   string         `yaml:"listen"`
		LogLevel string         `yaml:"log_level"`
		LogFile  string         `yaml:"log_file"`
		ModelsCacheFile       string `yaml:"models_cache_file"`
		ModelsRefreshInterval string `yaml:"models_refresh_interval"`
		Auth     AuthCfg        `yaml:"auth"`
		Routes   map[string]Route `yaml:"routes"`
		Takeover Takeover       `yaml:"takeover"`
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
	cfg.ModelsCacheFile = raw.ModelsCacheFile
	cfg.ModelsRefreshInterval = raw.ModelsRefreshInterval
	cfg.Auth = raw.Auth
	cfg.Takeover = raw.Takeover

	// Convert routes from a named map into a slice.
	for name, r := range raw.Routes {
		r.Name = name
		cfg.Routes = append(cfg.Routes, r)
	}

	// Expand auth paths.
	cfg.Auth.SSOCookieFile = expandPath(cfg.Auth.SSOCookieFile)
	cfg.Auth.CodexAuthFile = expandPath(cfg.Auth.CodexAuthFile)
	// Expand log path.
	cfg.LogFile = expandPath(cfg.LogFile)
	// Expand takeover paths.
	t := &cfg.Takeover
	t.ClaudeFile = expandPath(t.ClaudeFile)
	t.OpencodeFile = expandPath(t.OpencodeFile)
	t.CodexFile = expandPath(t.CodexFile)
	t.PiFile = expandPath(t.PiFile)
	// If proxy_url is unset, derive it from `listen` so changing the port only
	// requires editing `listen` (a common footgun: proxy_url pointing at the old
	// default port while listen moved). An explicit proxy_url always wins.
	if t.ProxyURL == "" && cfg.Listen != "" {
		t.ProxyURL = "http://" + cfg.Listen
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen is empty")
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Upstream == "" {
			return fmt.Errorf("route %s: upstream is empty", r.Name)
		}
		if len(r.PathPrefixes) == 0 {
			return fmt.Errorf("route %s: no path_prefixes", r.Name)
		}
	}
	return nil
}

// findRoute longest-prefix match.
func (c *Config) findRoute(path string) *Route {
	var best *Route
	bestLen := -1
	for i := range c.Routes {
		r := &c.Routes[i]
		for _, p := range r.PathPrefixes {
			if strings.HasPrefix(path, p) && len(p) > bestLen {
				best = r
				bestLen = len(p)
			}
		}
	}
	return best
}
