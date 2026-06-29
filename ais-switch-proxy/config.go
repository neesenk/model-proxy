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
	Auth     AuthCfg  `yaml:"auth"`
	Routes   []Route  `yaml:"routes"` // preserve order; yaml maps aren't ordered, so use a struct slice
	Takeover Takeover `yaml:"takeover"`
}

type AuthCfg struct {
	SSOCookieFile   string `yaml:"sso_cookie_file"`
	CQPMintURL      string `yaml:"cqp_mint_url"`
	StaticKey       string `yaml:"static_key"`
	GeminiAPIKeyEnv string `yaml:"gemini_api_key_env"`
}

type Route struct {
	Name          string            `yaml:"name"`
	PathPrefixes  []string          `yaml:"path_prefixes"`
	Upstream      string            `yaml:"upstream"`
	UpstreamPath  string            `yaml:"upstream_path"`
	Auth          string            `yaml:"auth"` // cqp | gemini_key | static | none
	ModelMap      map[string]string `yaml:"model_map"`
}

type Takeover struct {
	ProxyURL            string   `yaml:"proxy_url"`
	ClaudeFile          string   `yaml:"claude_file"`
	OpencodeFile        string   `yaml:"opencode_file"`
	OpencodeProviderID  string   `yaml:"opencode_provider_id"`
	CodexFile           string   `yaml:"codex_file"`
	PiFile              string   `yaml:"pi_file"`
	PiProviderName      string   `yaml:"pi_provider_name"`
	PiModels            []string `yaml:"pi_models"`
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
	cfg.Auth = raw.Auth
	cfg.Takeover = raw.Takeover

	// Convert routes from a named map into a slice.
	for name, r := range raw.Routes {
		r.Name = name
		cfg.Routes = append(cfg.Routes, r)
	}

	// Expand auth paths.
	cfg.Auth.SSOCookieFile = expandPath(cfg.Auth.SSOCookieFile)
	// Expand log path.
	cfg.LogFile = expandPath(cfg.LogFile)
	// Expand takeover paths.
	t := &cfg.Takeover
	t.ClaudeFile = expandPath(t.ClaudeFile)
	t.OpencodeFile = expandPath(t.OpencodeFile)
	t.CodexFile = expandPath(t.CodexFile)
	t.PiFile = expandPath(t.PiFile)

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
