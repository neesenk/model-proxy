package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"model-proxy/internal/pricing"
	"model-proxy/internal/protocol"

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
	Cache         CacheConfig              `yaml:"cache"`
	// Shadow maps an exposed model to a candidate backend to evaluate: each
	// committed request to the route is ALSO sent to the shadow provider (same
	// prompt, the shadow's model), logged for quality/latency comparison, and the
	// result is NOT returned to the client. Empty/missing = off. Requires
	// request_log to record shadow results.
	Shadow map[string]ShadowTarget `yaml:"shadow"`
	// ShadowSampleRate (*float64): nil = default 1.0 (all requests); explicit
	// 0.0 = shadowing OFF (distinguishes "unset" from "disabled"); 0.5 = half.
	ShadowSampleRate *float64 `yaml:"shadow_sample_rate"`
	// ShadowMaxConcurrent caps the number of in-flight shadow goroutines.
	// Default 4. Additional shadows are silently dropped (best-effort) when the
	// cap is reached, preventing goroutine explosion under high QPS.
	ShadowMaxConcurrent int `yaml:"shadow_max_concurrent"`
	// Fusion holds named multi-model orchestration recipes (panel → synthesis).
	// A route references one with {provider: fusion, model: <recipe name>}:
	// the request fans out to the panel in parallel, and the synthesizer model
	// answers the client from the collected drafts. Empty/missing = off.
	Fusion  map[string]FusionConfig `yaml:"fusion"`
	Pricing PricingConfig           `yaml:"pricing"`
	Prices  map[string]PriceConfig  `yaml:"prices"`
}

// FusionConfig is one multi-model orchestration recipe: a panel of 2..4 draft
// providers fanned out in parallel, plus a synthesizer that produces the final
// answer from the collected drafts. MinPanel is the quorum — the minimum number
// of drafts required before synthesis (default 2); below quorum the request
// degrades to a plain direct call to the synthesizer with the original body.
type FusionConfig struct {
	Panel       []RouteTarget `yaml:"panel"`
	Synthesizer RouteTarget   `yaml:"synthesizer"`
	MinPanel    int           `yaml:"min_panel"`
	// MaxRunsPerDay caps ORCHESTRATED runs per local day (0 = unlimited); an
	// over-budget request degrades to a plain direct synthesizer call
	// (budget_exceeded). Runs degraded before fan-out never consume the budget.
	MaxRunsPerDay int `yaml:"max_runs_per_day"`
	// FirstTurnOnly restricts orchestration to the first conversation turn: a
	// request body already carrying an assistant message goes straight to the
	// synthesizer (multi_turn) — multi-turn sessions would otherwise fan out
	// (and pay the N+1 cost) on every turn.
	FirstTurnOnly bool `yaml:"first_turn_only"`
	// Judge is an optional analysis step between quorum and synthesis: one
	// non-streaming call reviewing the candidates (consensus / conflicts /
	// omissions); its report is injected into the synthesis body. A judge
	// failure skips the report without degrading the run. nil = off.
	Judge *RouteTarget `yaml:"judge"`
	// Instruction overrides the fixed synthesis preamble (rune-capped, see
	// fusionInstructionMaxRunes). Empty = the built-in template.
	Instruction string `yaml:"instruction"`
}

// fusionInstructionMaxRunes caps FusionConfig.Instruction (validation-enforced)
// so a pasted essay can't silently bloat every synthesis prompt.
const fusionInstructionMaxRunes = 4000

// ShadowTarget names the candidate backend for shadow evaluation of a route.
type ShadowTarget struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// Protocol declares the shadow backend's protocol ("anthropic"|"openai"). Empty
	// = same as the request body's protocol (the primary target's backend proto).
	// Set it when the shadow backend speaks a different protocol than the body the
	// shadow request is built from — runShadow converts + routes accordingly.
	Protocol string `yaml:"protocol"`
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

// ResolvedDBPath returns the SQLite stats DB path, defaulting to
// ~/.model-proxy/stats.db.
func (s StatsConfig) ResolvedDBPath() string {
	if s.DBPath != "" {
		return ExpandPath(s.DBPath)
	}
	return filepath.Join(homeDir(), ".model-proxy", "stats.db")
}

// RetentionDuration returns the bucket retention duration, defaulting to 30 days.
// 0 or "0" keeps history forever.
func (s StatsConfig) RetentionDuration() time.Duration {
	if s.Retention == "" {
		return 720 * time.Hour
	}
	if d, err := time.ParseDuration(s.Retention); err == nil {
		return d
	}
	return 720 * time.Hour
}

// PricingConfig configures the analytics equivalent-cost pricing source
// (OpenRouter catalog, cached). Defaults: enabled, 24h TTL, the OpenRouter
// endpoint. enabled=false → no fetch; cost shows n/a everywhere.
type PricingConfig struct {
	Enabled   bool   `yaml:"enabled"`
	TTL       string `yaml:"ttl"`
	SourceURL string `yaml:"source_url"`
}

// IsEnabled reports whether catalog pricing is enabled.
func (p PricingConfig) IsEnabled() bool { return p.Enabled }

// TTLDuration returns the catalog cache TTL, defaulting to 24h.
func (p PricingConfig) TTLDuration() time.Duration {
	if p.TTL == "" {
		return pricing.DefaultTTL
	}
	if d, err := time.ParseDuration(p.TTL); err == nil {
		return d
	}
	return pricing.DefaultTTL
}

// ResolvedSourceURL returns the pricing endpoint. Precedence: config `source_url` >
// MP_PRICING_URL env (mirrors MP_MODELSDEV_URL) > OpenRouter default.
func (p PricingConfig) ResolvedSourceURL() string {
	if p.SourceURL != "" {
		return p.SourceURL
	}
	return pricingEndpoint() // MP_PRICING_URL env, else OpenRouter default
}

// pricingEndpoint returns the catalog endpoint, overridable via MP_PRICING_URL.
func pricingEndpoint() string {
	if value := os.Getenv("MP_PRICING_URL"); value != "" {
		return value
	}
	return pricing.DefaultEndpoint
}

// PriceConfig is a per-model price override in USD per MILLION tokens (human
// units); converted to USD/token at lookup (÷ 1e6). cache_read/cache_write
// default to 0.
type PriceConfig struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
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

// ResolvedDir returns the request-log directory, defaulting to
// ~/.model-proxy/requests.
func (r RequestLogConfig) ResolvedDir() string {
	if r.Dir != "" {
		return ExpandPath(r.Dir)
	}
	return filepath.Join(homeDir(), ".model-proxy", "requests")
}

// MaxFileSizeBytes returns the per-file rotation cap in bytes, defaulting to 1 GiB.
// Values <= 0 fall back to the default.
func (r RequestLogConfig) MaxFileSizeBytes() int64 {
	if r.MaxFileSize > 0 {
		return r.MaxFileSize
	}
	return 1 << 30 // 1 GiB
}

// MaxBodyBytesValue returns the per-body capture cap in bytes, defaulting to 5 MiB.
// Values <= 0 fall back to the default.
func (r RequestLogConfig) MaxBodyBytesValue() int {
	if r.MaxBodyBytes > 0 {
		return r.MaxBodyBytes
	}
	return 5 * 1024 * 1024
}

// RetentionDuration returns the rotated-file retention duration, defaulting to 30 days.
// "0" or a zero duration means keep forever (no sweep).
func (r RequestLogConfig) RetentionDuration() time.Duration {
	if r.Retention == "" {
		return 720 * time.Hour
	}
	if d, err := time.ParseDuration(r.Retention); err == nil {
		return d
	}
	return 720 * time.Hour
}

// CacheConfig configures the exact-match response cache.
type CacheConfig struct {
	Enabled      bool   `yaml:"enabled"`
	TTL          string `yaml:"ttl"`            // entry expiry (default 10m)
	MaxEntries   int    `yaml:"max_entries"`    // size cap (default 1000)
	MaxBodyBytes int    `yaml:"max_body_bytes"` // cache only responses ≤ this (default 256KiB)
}

// IsEnabled reports whether the exact-match response cache is enabled.
func (c CacheConfig) IsEnabled() bool { return c.Enabled }

// TTLDuration returns the cache entry TTL, defaulting to 10 minutes.
func (c CacheConfig) TTLDuration() time.Duration {
	if c.TTL == "" {
		return 10 * time.Minute
	}
	if d, err := time.ParseDuration(c.TTL); err == nil {
		return d
	}
	return 10 * time.Minute
}

// MaxEntriesValue returns the cache entry cap, defaulting to 1000.
func (c CacheConfig) MaxEntriesValue() int {
	if c.MaxEntries > 0 {
		return c.MaxEntries
	}
	return 1000
}

// MaxBodyBytesValue returns the largest cacheable response body, defaulting to
// 256 KiB.
func (c CacheConfig) MaxBodyBytesValue() int {
	if c.MaxBodyBytes > 0 {
		return c.MaxBodyBytes
	}
	return 256 * 1024
}

// Scheduling configures failover health (circuit breaker, rate-limit skip) and
// sticky routing. Durations are parsed from strings (e.g. "10m", "60s") via
// time.ParseDuration; unset/invalid values fall back to the defaults shown below.
type Scheduling struct {
	CircuitThreshold  int    `yaml:"circuit_threshold"`   // consecutive failures → open circuit (default 3)
	CircuitCooldown   string `yaml:"circuit_cooldown"`    // circuit open duration, then half-open 1 probe (default 10m)
	RateLimitBackoff  string `yaml:"rate_limit_backoff"`  // 429 with no Retry-After/hint, transient class: skip this long, then probe (default 60s)
	QuotaCooldown     string `yaml:"quota_cooldown"`      // 429 classified quota-exhausted with no reset hint: skip this long (default 1h; daily class locks to midnight)
	ModelLockout      string `yaml:"model_lockout"`       // model-level failure (404 / model-denied / empty 200): lock (provider,model) this long (default 10m)
	RetryWait         string `yaml:"retry_wait"`          // all targets cooling down: wait ≤ this for the earliest expiry and retry (≤2×) instead of an immediate error (default 10s; "0" disables)
	UpstreamTimeout   string `yaml:"upstream_timeout"`    // per-upstream-request timeout (default 1800s)
	StickyDwell       string `yaml:"sticky_dwell"`        // min time on the chosen provider before re-evaluating (default 10m)
	QuotaPollInterval string `yaml:"quota_poll_interval"` // background poll cadence (default 5m)
	QuotaSwitchMargin int    `yaml:"quota_switch_margin"` // switch if another plan provider's effective remaining beats current by ≥ this many pct points (default 15)
}

func (s Scheduling) Threshold() int {
	if s.CircuitThreshold > 0 {
		return s.CircuitThreshold
	}
	return 3
}
func (s Scheduling) Cooldown() time.Duration {
	if d, err := time.ParseDuration(s.CircuitCooldown); err == nil {
		return d
	}
	return 10 * time.Minute
}
func (s Scheduling) RateBackoff() time.Duration {
	if d, err := time.ParseDuration(s.RateLimitBackoff); err == nil {
		return d
	}
	return 60 * time.Second
}
func (s Scheduling) QuotaCooldownDuration() time.Duration {
	if d, err := time.ParseDuration(s.QuotaCooldown); err == nil {
		return d
	}
	return time.Hour
}
func (s Scheduling) ModelLockoutDuration() time.Duration {
	if d, err := time.ParseDuration(s.ModelLockout); err == nil {
		return d
	}
	return 10 * time.Minute
}
func (s Scheduling) RetryWaitDuration() time.Duration {
	if d, err := time.ParseDuration(s.RetryWait); err == nil {
		return d // "0" disables the cooldown wait-retry
	}
	return 10 * time.Second
}
func (s Scheduling) Timeout() time.Duration {
	if d, err := time.ParseDuration(s.UpstreamTimeout); err == nil {
		return d
	}
	return 1800 * time.Second
}
func (s Scheduling) Dwell() time.Duration {
	if d, err := time.ParseDuration(s.StickyDwell); err == nil {
		return d
	}
	return 10 * time.Minute
}
func (s Scheduling) PollInterval() time.Duration {
	if d, err := time.ParseDuration(s.QuotaPollInterval); err == nil {
		return d
	}
	return 5 * time.Minute
}
func (s Scheduling) SwitchMargin() float64 {
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
	// Capabilities is a manual per-model capability override — the escape hatch
	// for models the models.dev catalog doesn't know (codex/aqp/volcengine blind
	// spots). Keys are model names (validate requires them to appear in Models);
	// values are drawn from {image, tools}. For a DECLARED model the request-fit
	// check follows this list exactly ([image] = image yes, tools no), ignoring
	// the catalog; undeclared models keep catalog behavior. Context-window checks
	// always come from the catalog (capabilities declare no window).
	Capabilities map[string][]string `yaml:"capabilities"`
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

// PeakMultiplier returns the multiplier of whichever peak segment `now` falls
// into (1.0 if none / no segments). Used to discount effective remaining quota.
func (p Provider) PeakMultiplier(now time.Time) float64 {
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
			return DefaultPeakMultiplier
		}
	}
	return 1.0
}

const DefaultPeakMultiplier = 2.0

// parseHHMMRange parses a local-time window in HH:MM-HH:MM form.
func parseHHMMRange(value string) (start, end int, ok bool) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, startOK := parseHHMM(parts[0])
	end, endOK := parseHHMM(parts[1])
	if !startOK || !endOK {
		return 0, 0, false
	}
	return start, end, true
}

// parseHHMM parses a local wall-clock time and returns minutes since midnight.
func parseHHMM(value string) (int, bool) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// RouteTarget is one upstream destination for an exposed model name. A route maps
// an exposed name to an ordered list of targets; the proxy picks one by scheduling
// (by tier/quota band, peak folded into effective remaining) and fails over to the
// next on error.
type RouteTarget struct {
	Provider string `yaml:"provider"` // config providers[] key
	Model    string `yaml:"model"`    // real model name at that provider
	Priority int    `yaml:"priority"` // lower = tried first within a tier/quota band (default 0)
	// Protocol declares the backend wire protocol ("anthropic", "openai", or
	// "responses"). Empty means the client protocol is forwarded unchanged.
	// Set it only when the target requires cross-protocol conversion; the exact
	// conversion contract belongs to internal/protocol.
	Protocol string `yaml:"protocol"`
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

// ExpandPath expands ~ and the env: prefix.
func ExpandPath(p string) string {
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

func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
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
		Cache         CacheConfig              `yaml:"cache"`
		Shadow        map[string]ShadowTarget  `yaml:"shadow"`
		// Must mirror Config's shadow knobs — without these the file-loaded
		// values are silently dropped (and validate's range checks never fire).
		ShadowSampleRate    *float64                `yaml:"shadow_sample_rate"`
		ShadowMaxConcurrent int                     `yaml:"shadow_max_concurrent"`
		Fusion              map[string]FusionConfig `yaml:"fusion"`
		Pricing             PricingConfig           `yaml:"pricing"`
		Prices              map[string]PriceConfig  `yaml:"prices"`
	}
	raw := rawConfig{
		Listen:   "127.0.0.1:15721",
		LogLevel: "info",
		Web:      WebConfig{Enabled: true},
		Pricing:  PricingConfig{Enabled: true},
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
	cfg.Cache = raw.Cache
	cfg.Shadow = raw.Shadow
	cfg.ShadowSampleRate = raw.ShadowSampleRate
	cfg.ShadowMaxConcurrent = raw.ShadowMaxConcurrent
	cfg.Fusion = raw.Fusion
	cfg.Pricing = raw.Pricing
	cfg.Prices = raw.Prices
	cfg.LogFile = ExpandPath(cfg.LogFile)
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
	t.Claude = ExpandPath(t.Claude)
	t.Opencode = ExpandPath(t.Opencode)
	t.Codex = ExpandPath(t.Codex)
	t.Pi = ExpandPath(t.Pi)
	if t.ProxyURL == "" && cfg.Listen != "" {
		t.ProxyURL = "http://" + cfg.Listen
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// requireLoopbackListen rejects non-loopback listen addresses. /api/* and /ui/
// are unauthenticated, so binding to anything but loopback exposes config
// editing and account management to the whole network.
func requireLoopbackListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q is invalid (%v) — use 127.0.0.1:PORT", listen, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen %q is not loopback — /api/* and /ui/ have no auth, refusing to expose them; use 127.0.0.1:PORT", listen)
}

// validate returns nil if the config is valid. On failure it returns an error
// with a human-readable message including a hint for fixing the issue.
func (c *Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen is empty — set `listen: 127.0.0.1:PORT` in config")
	}
	if err := requireLoopbackListen(c.Listen); err != nil {
		return err
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured — add at least one under `providers:`")
	}
	for name, p := range c.Providers {
		// A provider needs at least one upstream base URL. openai_base_url is the
		// default (same-protocol openai forwarding); anthropic_base_url is used by
		// anthropic same-protocol forwarding AND protocol:anthropic conversion
		// targets. Requiring openai_base_url specifically would force a dummy value
		// on pure-anthropic backends, so accept either.
		if p.OpenAIBaseURL == "" && p.AnthropicBaseURL == "" {
			return fmt.Errorf("provider %q: set at least one of openai_base_url / anthropic_base_url", name)
		}
		if p.Provider == "" {
			return fmt.Errorf("provider %q: provider_id is empty — set `provider_id:` (e.g. zhipu, aqp, codex, deepseek, volcengine, qwen-plan)", name)
		}
		// Check for known provider_id typos. apikey is a credential *category*
		// (zhipu/deepseek/volcengine/kimi-code), not a registered provider_id;
		// static's login eligibility comes from its API-key pool. The generic
		// `headers` map is applied later as an override, but cannot make an
		// uncredentialed static provider runnable.
		known := map[string]bool{"aqp": true, "codex": true, "zhipu": true, "deepseek": true, "volcengine": true, "kimi-code": true, "static": true, "zcode": true, "qwen-plan": true}
		if !known[p.Provider] {
			return fmt.Errorf("provider %q: unknown provider_id %q — valid: aqp, codex, zhipu, deepseek, volcengine, kimi-code, static, zcode, qwen-plan", name, p.Provider)
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
		// capabilities: keys must name a model in models: (anything else is
		// almost certainly a typo that would silently never match); values must
		// be known capability names.
		for model, caps := range p.Capabilities {
			inModels := false
			for _, m := range p.Models {
				if m == model {
					inModels = true
					break
				}
			}
			if !inModels {
				return fmt.Errorf("provider %q: capabilities key %q is not in its models: list — likely a typo; add the model to models: or fix the key", name, model)
			}
			for _, c := range caps {
				if c != "image" && c != "tools" {
					return fmt.Errorf("provider %q: capabilities[%q]: unknown capability %q — valid values: image, tools", name, model, c)
				}
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
			// provider "fusion" is not a real provider — it references a recipe
			// under fusion: by recipe name (the target's model field). Checked here
			// because the generic provider-exists check below would reject it.
			if t.Provider == "fusion" {
				if _, ok := c.Fusion[t.Model]; !ok {
					return fmt.Errorf("route %q target %d: fusion recipe %q not defined under fusion: — add a `fusion: %s:` recipe or fix the model name", exposed, i, t.Model, t.Model)
				}
				if t.Protocol != "" {
					return fmt.Errorf("route %q target %d: provider \"fusion\" takes no protocol — set protocol per panel member / synthesizer inside the recipe", exposed, i)
				}
				continue
			}
			if _, ok := c.Providers[t.Provider]; !ok {
				return fmt.Errorf("route %q target %d: provider %q not defined under providers: — check spelling or add the provider", exposed, i, t.Provider)
			}
			// Protocol conversion (#11): a target declaring protocol:anthropic
			// needs the provider's anthropic_base_url (and protocol:openai /
			// protocol:responses need openai_base_url — responses reuses the
			// OpenAI base, e.g. codex's openai_base_url is its /responses
			// endpoint); without it the converted request has no upstream base
			// URL and 400s at runtime. Catch it at validate time.
			if t.Protocol != "" {
				prov := c.Providers[t.Provider]
				if err := checkTargetProtocol(fmt.Sprintf("route %q target %d", exposed, i), t, prov); err != nil {
					return err
				}
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
	// Shadow validation: each entry references a real route + provider + valid
	// protocol; sample rate in [0,1]; max_concurrent >= 0.
	for route, sh := range c.Shadow {
		if _, ok := c.Routes[route]; !ok {
			return fmt.Errorf("shadow %q: route not found in routes: — add a route named %q", route, route)
		}
		if sh.Provider == "fusion" {
			return fmt.Errorf("shadow %q: provider \"fusion\" is not a valid shadow target — shadow a concrete provider", route)
		}
		prov, ok := c.Providers[sh.Provider]
		if !ok {
			return fmt.Errorf("shadow %q: provider %q not defined under providers:", route, sh.Provider)
		}
		if sh.Protocol != "" {
			target := RouteTarget{Provider: sh.Provider, Model: sh.Model, Protocol: sh.Protocol}
			if err := checkTargetProtocol(fmt.Sprintf("shadow %q", route), target, prov); err != nil {
				return err
			}
		}
	}
	// Fusion validation: each recipe's panel/synthesizer reference real providers
	// (no nesting), the panel has 2..4 members, and min_panel fits the panel.
	for name, f := range c.Fusion {
		if len(f.Panel) < 2 || len(f.Panel) > 4 {
			return fmt.Errorf("fusion %q: panel must have 2..4 members (got %d)", name, len(f.Panel))
		}
		if f.MinPanel < 0 || f.MinPanel > len(f.Panel) {
			return fmt.Errorf("fusion %q: min_panel %d out of range [0, %d] (panel size)", name, f.MinPanel, len(f.Panel))
		}
		if f.MaxRunsPerDay < 0 {
			return fmt.Errorf("fusion %q: max_runs_per_day %d out of range (0 = unlimited)", name, f.MaxRunsPerDay)
		}
		if utf8.RuneCountInString(f.Instruction) > fusionInstructionMaxRunes {
			return fmt.Errorf("fusion %q: instruction is %d runes, max %d", name, utf8.RuneCountInString(f.Instruction), fusionInstructionMaxRunes)
		}
		for i, m := range f.Panel {
			if err := c.checkFusionTarget(name, fmt.Sprintf("panel %d", i), m); err != nil {
				return err
			}
		}
		if err := c.checkFusionTarget(name, "synthesizer", f.Synthesizer); err != nil {
			return err
		}
		// The judge follows the same member rules (real provider, no nesting —
		// checkFusionTarget already rejects provider "fusion").
		if f.Judge != nil {
			if err := c.checkFusionTarget(name, "judge", *f.Judge); err != nil {
				return err
			}
		}
	}
	if c.ShadowSampleRate != nil {
		if r := *c.ShadowSampleRate; r < 0 || r > 1 {
			return fmt.Errorf("shadow_sample_rate %v out of range [0, 1]", r)
		}
	}
	if c.ShadowMaxConcurrent < 0 {
		return fmt.Errorf("shadow_max_concurrent %d must be >= 0", c.ShadowMaxConcurrent)
	}
	// NOTE: duplicate priorities within a route are intentionally allowed. The
	// scheduler (proxy.go decideOrder) ranks by tier -> priority -> surplus, so
	// same-priority targets form a surplus-competed pool (higher-surplus wins;
	// sticky switching only on a margin edge). Do NOT re-add a duplicate-priority
	// rejection here - it contradicts the scheduling contract (AGENTS.md
	// "quota-aware scheduling").
	return nil
}

// checkFusionTarget validates one fusion recipe member (panel member or
// synthesizer): the provider exists (and is not itself fusion — no nesting),
// the model is set, and a declared protocol is servable by the provider's base
// URLs (same rule as route targets).
func (c *Config) checkFusionTarget(recipe, where string, t RouteTarget) error {
	what := fmt.Sprintf("fusion %q %s", recipe, where)
	if t.Provider == "" {
		return fmt.Errorf("%s: provider is empty", what)
	}
	if t.Provider == "fusion" {
		return fmt.Errorf("%s: nested fusion recipes are not supported", what)
	}
	prov, ok := c.Providers[t.Provider]
	if !ok {
		return fmt.Errorf("%s: provider %q not defined under providers:", what, t.Provider)
	}
	if t.Model == "" {
		return fmt.Errorf("%s: model is empty", what)
	}
	return checkTargetProtocol(what, t, prov)
}

// checkTargetProtocol validates a target's declared backend protocol against
// the closed wire-protocol set — protocol.Parse is
// the single source of truth for the legal values — and against the provider's
// base URLs: protocol:anthropic needs anthropic_base_url, openai/responses
// need openai_base_url (responses reuses the OpenAI base, e.g. codex's
// openai_base_url is its /responses endpoint). `what` is the caller's error
// prefix (e.g. `route "glm" target 0` / `fusion "f" synthesizer`).
func checkTargetProtocol(what string, t RouteTarget, prov Provider) error {
	if t.Protocol == "" {
		return nil
	}
	wireProtocol, ok := protocol.Parse(t.Protocol)
	if !ok {
		return fmt.Errorf("%s: protocol %q is not \"anthropic\", \"openai\", or \"responses\"", what, t.Protocol)
	}
	switch wireProtocol {
	case protocol.Anthropic:
		if prov.AnthropicBaseURL == "" {
			return fmt.Errorf("%s: protocol:anthropic but provider %q has no anthropic_base_url — conversion needs it", what, t.Provider)
		}
	case protocol.OpenAI, protocol.Responses:
		if prov.OpenAIBaseURL == "" {
			return fmt.Errorf("%s: protocol:%s but provider %q has no openai_base_url — conversion needs it", what, t.Protocol, t.Provider)
		}
	}
	return nil
}

// ProviderConfig resolves the Provider config for name, resolving a
// credential-pool virtual id ("name#<accountID>") back to its parent. parentOf
// is the snapshot taken alongside the config; for a non-virtual name (incl.
// single-account providers), parentOf[name] is "" and the config is read
// directly. Returns the zero Provider (ok=false) if neither name nor a parent
// is found — callers treat that as an unknown provider.
func ProviderConfig(cfg *Config, parentOf map[string]string, name string) (Provider, bool) {
	if parent := parentOf[name]; parent != "" {
		name = parent
	}
	p, ok := cfg.Providers[name]
	return p, ok
}
