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
	// Providers defines upstream backends (baseURL + auth + models). Referenced
	// by name from routes. Supports external providers (static apiKey) and
	// managed ones (PROXY_MANAGED → proxy injects real credentials).
	Providers map[string]Provider `yaml:"providers"`
	// Routes maps protocol name (anthropic | openai) to its model→provider/model
	// mapping. The proxy exposes each protocol at its standard path
	// (/v1/messages for anthropic, /v1/chat/completions + /v1/responses for
	// openai) and forwards to the provider using the SAME protocol (no conversion).
	Routes   map[string]ProtocolRoute `yaml:"routes"`
	Takeover Takeover `yaml:"takeover"`
}

type AuthCfg struct {
	SSOCookieFile string `yaml:"sso_cookie_file"`
	CQPMintURL    string `yaml:"cqp_mint_url"`
	StaticKey     string `yaml:"static_key"`
	// CodexAuthFile is read by the codex_oauth auth provider to get the ChatGPT
	// access/refresh tokens for the codex native backend.
	CodexAuthFile string `yaml:"codex_auth_file"`
}

// Provider is an upstream backend definition: baseURL + auth + models.
// Referenced by name from routes. A provider's baseURL is the API base (e.g.
// .../compass-api/v1); the proxy appends the protocol-specific path (/messages
// for anthropic, /responses or /chat/completions for openai) when forwarding.
type Provider struct {
	BaseURL  string                   `yaml:"baseURL"`
	Provider string                   `yaml:"provider_id"` // compass | codex | zhipu | deepseek | apikey
	Headers  map[string]string        `yaml:"headers"`
	UsageURL string                   `yaml:"usageURL"`
	Models   map[string]ProviderModel `yaml:"models"`
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

// ProtocolRoute maps exposed model names to "provider/model" for one protocol.
// The protocol determines the path the proxy listens on and forwards to:
//   anthropic → /v1/messages (client) → provider /messages
//   openai    → /v1/responses, /v1/chat/completions (client) → provider same
type ProtocolRoute struct {
	Models map[string]string `yaml:"models"` // exposed name → "provider/model"
}

type Takeover struct {
	ProxyURL     string                `yaml:"proxy_url"`
	ClaudeFile   string                `yaml:"claude_file"`
	OpencodeFile string                `yaml:"opencode_file"`
	CodexFile    string                `yaml:"codex_file"`
	PiFile       string                `yaml:"pi_file"`
	// ProviderID is the single provider identifier used by takeover for every
	// agent that takes one (opencode, pi, codex, and future agents). claude
	// doesn't use it (it writes env vars). Default "model-proxy".
	ProviderID   string                `yaml:"provider_id"`
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
		Listen   string         `yaml:"listen"`
		LogLevel string         `yaml:"log_level"`
		LogFile  string         `yaml:"log_file"`
		ModelsCacheFile       string `yaml:"models_cache_file"`
		ModelsRefreshInterval string `yaml:"models_refresh_interval"`
		Auth     AuthCfg        `yaml:"auth"`
		Providers map[string]Provider      `yaml:"providers"`
		Routes    map[string]ProtocolRoute `yaml:"routes"`
		Takeover  Takeover                 `yaml:"takeover"`
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
	cfg.Providers = raw.Providers
	cfg.Routes = raw.Routes
	cfg.Takeover = raw.Takeover

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
	// If proxy_url is unset, derive it from `listen`.
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
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	for name, p := range c.Providers {
		if p.BaseURL == "" {
			return fmt.Errorf("provider %s: baseURL is empty", name)
		}
		if p.Provider == "" {
			return fmt.Errorf("provider %s: auth is empty", name)
		}
	}
	for proto, route := range c.Routes {
		for exposed, target := range route.Models {
			parts := strings.SplitN(target, "/", 2)
			if len(parts) != 2 {
				return fmt.Errorf("route %s: model %s → %q must be provider/model", proto, exposed, target)
			}
			if _, ok := c.Providers[parts[0]]; !ok {
				return fmt.Errorf("route %s: model %s references unknown provider %q", proto, exposed, parts[0])
			}
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
