package serve

import "testing"

// TestApplyDefaultMemoryLimit pins the default's two contracts: an unset
// GOMEMLIMIT gets the 1GiB soft ceiling, and an operator-provided GOMEMLIMIT
// is never overridden.
func TestApplyDefaultMemoryLimit(t *testing.T) {
	if !applyDefaultMemoryLimit(func(string) string { return "" }, func(limit int64) int64 {
		if limit != DefaultMemoryLimitBytes {
			t.Errorf("applied limit = %d, want %d", limit, DefaultMemoryLimitBytes)
		}
		return 0
	}) {
		t.Error("unset GOMEMLIMIT must apply the default limit")
	}

	if applyDefaultMemoryLimit(func(string) string { return "512MiB" }, func(int64) int64 {
		t.Error("operator GOMEMLIMIT must not be overridden")
		return 0
	}) {
		t.Error("set GOMEMLIMIT must report no default applied")
	}
}
