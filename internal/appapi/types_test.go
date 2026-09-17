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

func (fakeReads) Dashboard(time.Time) Dashboard             { return Dashboard{} }
func (fakeReads) LogFile() string                           { return "" }
func (fakeReads) RequestLogDirectory() string               { return "" }
func (fakeReads) RequestLogQueries() RequestLogQueries      { return nil }
func (fakeReads) Accounts() []ProviderAccounts              { return nil }
func (fakeReads) Tokens(int64, int64) ([]TokenUsage, error) { return nil, nil }
func (fakeReads) Agents(int64, int64) ([]AgentUsage, error) { return nil, nil }
func (fakeReads) StatsSince() int64                         { return 0 }
func (fakeReads) Stats(StatsQuery) ([]observestats.Bucket, error) {
	return nil, nil
}
func (fakeReads) AgentStats(AgentStatsQuery) ([]observestats.AgentBucket, error) {
	return nil, nil
}
func (fakeReads) Analytics(AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
	return nil, nil
}
func (fakeReads) AnalyticsAgentNames(AnalyticsQuery) []string { return nil }
func (fakeReads) Pricing() PricingSnapshot                    { return PricingSnapshot{} }
func (fakeReads) Fusion(string, time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	return nil, nil
}
func (fakeReads) Pins() []Pin { return nil }
func (fakeReads) Security(SecurityQuery) (SecurityResult, error) {
	return SecurityResult{}, nil
}
func (fakeReads) SecurityExplain(string, string, []string) (SecurityExplainResult, error) {
	return SecurityExplainResult{}, nil
}
func (fakeReads) ConfigDocument() (ConfigDocument, error) {
	return ConfigDocument{}, nil
}
func (fakeReads) ModelsDocument() ModelsDocument { return ModelsDocument{} }

type fakeCommands struct{}

func (fakeCommands) ResetStats() error                         { return nil }
func (fakeCommands) RefreshQuota(string) bool                  { return false }
func (fakeCommands) ResetHealth(string) ([]string, int, error) { return nil, 0, nil }
func (fakeCommands) FreezeHealth(string) ([]string, error)     { return nil, nil }
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

func (fakeReads) Presets() []presets.Preset       { return nil }
func (fakeReads) MCPSurface() MCPSurface          { return MCPSurface{} }
func (fakeReads) SecurityBlocks() []SecurityBlock { return nil }
func (fakeReads) SecurityAdjudications() SecurityAdjudicationFeed {
	return SecurityAdjudicationFeed{Adjudications: []SecurityAdjudication{}}
}
func (fakeCommands) SecurityUnblock(string) error { return nil }

func (fakeCommands) AddPreset(string) ([]string, string, error) { return nil, "", nil }
func (fakeCommands) RefreshModels(context.Context, string) (ModelsRefreshResult, error) {
	return ModelsRefreshResult{}, nil
}

func (fakeCommands) ProbeMCP(context.Context, string) (MCPProbeResult, error) {
	return MCPProbeResult{}, nil
}
