package provider

import (
	"encoding/json"
	"fmt"
	"time"
)

// ZhipuProvider implements the Zhipu BigModel provider using an API key
// stored in ~/.model-proxy/<providerName>_apikey.json.
type ZhipuProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

func init() {
	Register("zhipu", func(cfg *Config, providerName string) (Provider, error) {
		return &ZhipuProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// newApiKeyBaseBound returns a bound ApiKeyBase (in-memory key) when cfg has a
// BoundAPIKey (credential-pool virtual), otherwise the legacy file-backed base.
// Shared by zhipu/deepseek/volcengine constructors.
func newApiKeyBaseBound(cfg *Config, providerName string) *ApiKeyBase {
	if cfg.BoundAPIKey != "" {
		return NewApiKeyBaseWithKey(providerName, cfg.BoundAPIKey)
	}
	return NewApiKeyBase(providerName)
}

func (p *ZhipuProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body // no special rewriting
}

func (p *ZhipuProvider) Logout() error {
	return p.DeleteKey()
}

func (p *ZhipuProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// Quota GETs the zhipu usage_url and parses the BigModel quota envelope. On any
// failure (auth, HTTP, non-zhipu body) returns a BillingUnknown snapshot
// carrying the error (never a non-nil error) so the poll stays alive.
func (p *ZhipuProvider) Quota() (*QuotaSnapshot, error) {
	return bigmodelQuota(p.cfg.UsageURL, p.AuthHeaders, p.cfg.Headers)
}

// ParseZhipuQuota parses Zhipu BigModel's /api/monitor/usage/quota/limit body into
// a QuotaSnapshot. Returns (nil, nil) if the body isn't the zhipu quota format
// (caller falls back to the OpenAI model-list display). TIME_LIMIT windows are
// included for display but EXCLUDED from the binding RemainingPct (they're MCP
// tool quota, not LLM tokens).
//
// Zhipu's `percentage` field is the USED percentage (0..100): the existing
// pre-refactor display fed it straight to progressBar/`%d%% used`, and the
// quota/limit fixture corroborates (percentage=40 ⇔ currentValue=40000 /
// usage=100000). We convert to RemainingPct = (100 - percentage) / 100.
func ParseZhipuQuota(body []byte, account string) (*QuotaSnapshot, error) {
	var z struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Limits []struct {
				Type          string `json:"type"`
				Unit          int    `json:"unit"`
				Number        int    `json:"number"`
				Percentage    int    `json:"percentage"`
				NextResetTime int64  `json:"nextResetTime"`
				Usage         *int   `json:"usage"`
				CurrentValue  *int   `json:"currentValue"`
				Remaining     *int   `json:"remaining"`
				UsageDetails  []struct {
					ModelCode string `json:"modelCode"`
					Usage     int    `json:"usage"`
				} `json:"usageDetails"`
			} `json:"limits"`
			Level string `json:"level"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &z); err != nil {
		return nil, nil
	}
	if !z.Success || len(z.Data.Limits) == 0 {
		return nil, nil
	}
	s := &QuotaSnapshot{
		Billing: BillingPlan,
		Account: account,
		Level:   z.Data.Level,
		Plan:    z.Data.Level,
		AsOf:    time.Now(),
	}
	for _, l := range z.Data.Limits {
		w := QuotaWindow{
			Label:        zhipuLimitLabel(l.Type, l.Unit),
			RemainingPct: (100.0 - float64(l.Percentage)) / 100.0,
		}
		if l.Type == "TIME_LIMIT" {
			w.Kind = "time"
			w.DetailLabel = "By MCP tool"
		} else {
			w.Kind = "tokens"
			w.DetailLabel = "By model"
			// TOKENS_LIMIT windows: unit 3 = 5h (immediate rate cap), unit 6 = weekly (total budget).
			switch l.Unit {
			case 3:
				w.Short = true
				w.Duration = 5 * time.Hour
			case 6:
				w.Ultimate = true
				w.Duration = 7 * 24 * time.Hour
			}
		}
		if l.CurrentValue != nil && l.Remaining != nil {
			w.Used = float64(*l.CurrentValue)
			w.Total = float64(*l.CurrentValue + *l.Remaining)
		}
		if l.NextResetTime > 0 {
			w.ResetsAt = time.UnixMilli(l.NextResetTime)
		}
		for _, ud := range l.UsageDetails {
			w.Details = append(w.Details, QuotaDetail{Label: ud.ModelCode, Used: float64(ud.Usage)})
		}
		s.Windows = append(s.Windows, w)
	}
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s, nil
}

// zhipuLimitLabel maps a zhipu limit {type, unit} to a display label.
func zhipuLimitLabel(typ string, unit int) string {
	switch typ {
	case "TOKENS_LIMIT":
		switch unit {
		case 3:
			return "5h tokens"
		case 6:
			return "Weekly tokens"
		}
		return fmt.Sprintf("Tokens (unit=%d)", unit)
	case "TIME_LIMIT":
		switch unit {
		case 5:
			return "Monthly time"
		}
		return fmt.Sprintf("Time (unit=%d)", unit)
	}
	return fmt.Sprintf("%s (unit=%d)", typ, unit)
}
