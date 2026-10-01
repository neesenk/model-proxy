package runtime

import (
	"net/http"
	"time"

	"model-proxy/internal/provider"
)

// quotaFake is the shared minimal Provider fake for quota tests. The quota
// behavior is injected as a closure, so fixed snapshots, windowed snapshots,
// flaky failure scripts, ETA advance, and blocking (lifecycle) providers all
// share one ten-method implementation instead of per-file copies.
type quotaFake struct {
	quota func() (*provider.QuotaSnapshot, error)
}

func (f *quotaFake) AuthHeaders(*http.Request) error                        { return nil }
func (f *quotaFake) Refresh() error                                         { return nil }
func (f *quotaFake) RewriteRequest(string, []byte, string) (string, []byte) { return "", nil }
func (f *quotaFake) Logout() error                                          { return nil }
func (f *quotaFake) Usage() error                                           { return nil }
func (f *quotaFake) FetchModels() ([]string, error)                         { return nil, nil }
func (f *quotaFake) Quota() (*provider.QuotaSnapshot, error)                { return f.quota() }
func (f *quotaFake) ProbeRequest(string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (f *quotaFake) ExtraHeaders(*http.Request, []byte, string, string)   {}
func (f *quotaFake) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

// planQuota serves a fresh plan snapshot with the given remaining fraction.
func planQuota(rem float64) *quotaFake {
	return &quotaFake{quota: func() (*provider.QuotaSnapshot, error) {
		return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: rem, AsOf: time.Now()}, nil
	}}
}

// fixedQuota serves a copy of the given snapshot on every poll.
func fixedQuota(s *provider.QuotaSnapshot) *quotaFake {
	return &quotaFake{quota: func() (*provider.QuotaSnapshot, error) {
		cp := *s
		return &cp, nil
	}}
}
