package runtime

// manager_quota_rename_test.go — coverage for the reload-boundary quota
// carryover: a bare-name snapshot moves onto the pooled virtual key ahead of
// the generation swap's live-key pruning, and an existing virtual entry (a
// fresher observation for the same account) wins over the carried legacy one.

import (
	"testing"
	"time"

	"model-proxy/internal/provider"
)

func TestRenameQuotaKeys(t *testing.T) {
	now := time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	m.quotas["solo"] = scheduleQuota(provider.BillingPlan, 0.42, now)
	m.quotas["both"] = scheduleQuota(provider.BillingPlan, 0.10, now)
	m.quotas["both#acc"] = scheduleQuota(provider.BillingPlan, 0.90, now)

	m.RenameQuotaKeys(map[string]string{
		"solo":   "solo#acc",
		"both":   "both#acc",
		"absent": "absent#acc",
	})

	if got := m.Quota("solo#acc"); got == nil || got.RemainingPct != 0.42 {
		t.Fatalf("carried snapshot = %+v, want remaining 0.42 under solo#acc", got)
	}
	if leaked := m.Quota("solo"); leaked != nil {
		t.Fatalf("bare solo entry survived: %+v", leaked)
	}
	if got := m.Quota("both#acc"); got == nil || got.RemainingPct != 0.90 {
		t.Fatalf("existing virtual entry must win: %+v, want remaining 0.90", got)
	}
	if leaked := m.Quota("both"); leaked != nil {
		t.Fatalf("bare both entry survived: %+v", leaked)
	}

	// Degenerate inputs are no-ops.
	m.RenameQuotaKeys(nil)
	m.RenameQuotaKeys(map[string]string{"solo": "solo"})
	if got := m.Quota("solo#acc"); got == nil {
		t.Fatal("no-op rename lost the carried snapshot")
	}
}
