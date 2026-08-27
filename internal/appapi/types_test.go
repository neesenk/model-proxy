package appapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"model-proxy/internal/fusion"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/presets"
)

func TestHTTPError(t *testing.T) {
	err := NewHTTPError(503, "unavailable")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatal("NewHTTPError must unwrap to *HTTPError")
	}
	if httpErr.Status != 503 || httpErr.Error() != "unavailable" {
		t.Errorf("HTTPError = %+v", httpErr)
	}
	var nilErr *HTTPError
	if nilErr.Error() != "" {
		t.Error("nil HTTPError must render empty")
	}
}

func TestRequirePorts(t *testing.T) {
	if err := RequirePorts(nil, nil); err == nil {
		t.Error("nil reads must error")
	}
	if err := RequirePorts(fakeReads{}, nil); err == nil {
		t.Error("nil commands must error")
	}
	if err := RequirePorts(fakeReads{}, fakeCommands{}); err != nil {
		t.Errorf("valid ports: %v", err)
	}
}

// fakeReads/fakeCommands satisfy the port interfaces with zero values.
type fakeReads struct{}

func (fakeReads) Dashboard(time.Time) Dashboard { return Dashboard{} }
func (fakeReads) LogFile() string               { return "" }
func (fakeReads) RequestLogDirectory() string   { return "" }
func (fakeReads) Accounts() []ProviderAccounts  { return nil }
func (fakeReads) Tokens() []TokenUsage          { return nil }
func (fakeReads) Stats(StatsQuery) ([]observestats.Bucket, error) {
	return nil, nil
}
func (fakeReads) AgentStats(AgentStatsQuery) ([]observestats.AgentBucket, error) {
	return nil, nil
}
func (fakeReads) Analytics(AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
	return nil, nil
}
func (fakeReads) Pricing() PricingSnapshot { return PricingSnapshot{} }
func (fakeReads) Fusion(string, time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	return nil, nil
}
func (fakeReads) Pins() []Pin { return nil }
func (fakeReads) ConfigDocument() (ConfigDocument, error) {
	return ConfigDocument{}, nil
}

type fakeCommands struct{}

func (fakeCommands) ResetStats() error                         { return nil }
func (fakeCommands) RefreshQuota(string) bool                  { return false }
func (fakeCommands) ResetHealth(string) ([]string, int, error) { return nil, 0, nil }
func (fakeCommands) SetPin(string, string, time.Duration) (Pin, bool) {
	return Pin{}, false
}
func (fakeCommands) ClearPin(string) bool    { return false }
func (fakeCommands) SaveConfig([]byte) error { return nil }
func (fakeCommands) ValidateConfig([]byte) []ValidationIssue {
	return nil
}
func (fakeCommands) EditConfig(EditRequest) error {
	return nil
}
func (fakeCommands) AddAccount(context.Context, string, AccountInput) (MutationResult, error) {
	return MutationResult{}, nil
}
func (fakeCommands) ProbeAccount(context.Context, string, string) (ProbeResult, error) {
	return ProbeResult{}, nil
}
func (fakeCommands) RemoveAccount(string, string) (MutationResult, error) {
	return MutationResult{}, nil
}
func (fakeCommands) BeginLogin(context.Context, string) (LoginStart, error) {
	return LoginStart{}, nil
}

func (fakeReads) Presets() []presets.Preset { return nil }

func (fakeCommands) AddPreset(string) ([]string, error) { return nil, nil }
