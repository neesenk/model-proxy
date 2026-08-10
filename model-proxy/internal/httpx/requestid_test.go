package httpx

import "testing"

func TestNewRequestID(t *testing.T) {
	first, second := NewRequestID(), NewRequestID()
	if len(first) != 32 {
		t.Errorf("NewRequestID length = %d, want 32", len(first))
	}
	if first == second {
		t.Error("two NewRequestID calls must differ")
	}
	for _, c := range first {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("NewRequestID has non-hex char %q", c)
		}
	}
}
