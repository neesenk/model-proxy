package accounts

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSetLabel persists a rename under the pool lock and leaves the account ID
// (the identity that keys usage history and virtual runtime keys) untouched.
func TestSetLabel(t *testing.T) {
	home := t.TempDir()
	store := NewStore(home)
	idA := AccountID("zhipu", Credentials{APIKey: "ka"})
	idB := AccountID("zhipu", Credentials{APIKey: "kb"})
	if err := store.Save("zhipu", "zhipu", Pool{Version: 1, Accounts: []Account{
		{ID: idA, Label: "ka", APIKey: "ka", AddedAt: "2026-07-15T00:00:00Z"},
		{ID: idB, Label: "kb", APIKey: "kb", AddedAt: "2026-10-14T01:31:36Z"},
	}}); err != nil {
		t.Fatal(err)
	}

	if err := store.SetLabel("zhipu", "zhipu", idA, "  work horse  "); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	pool, err := store.Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 2 {
		t.Fatalf("pool accounts = %d, want 2 (rename must not add/drop accounts)", len(pool.Accounts))
	}
	byID := map[string]Account{}
	for _, a := range pool.Accounts {
		byID[a.ID] = a
	}
	if got := byID[idA].Label; got != "work horse" {
		t.Errorf("label = %q, want trimmed %q", got, "work horse")
	}
	if byID[idA].APIKey != "ka" || byID[idB].Label != "kb" || byID[idB].APIKey != "kb" {
		t.Errorf("rename touched unrelated fields: %+v", byID)
	}

	// Unknown id: fail-closed, pool untouched.
	if err := store.SetLabel("zhipu", "zhipu", "nope", "x"); err == nil || !strings.Contains(err.Error(), "unknown account") {
		t.Errorf("unknown id err = %v, want unknown-account failure", err)
	}
	// Invalid labels: rejected before any lock/pool work.
	for _, bad := range []string{"", "   ", strings.Repeat("x", MaxLabelLen+1)} {
		if err := store.SetLabel("zhipu", "zhipu", idA, bad); err == nil {
			t.Errorf("label %q accepted, want rejection", bad)
		}
	}
	// A provider without a plural pool (legacy singular) has no pool to rename
	// in — fail-closed instead of creating one.
	if err := store.SetLabel("legacy", "zhipu", idA, "x"); err == nil {
		t.Error("legacy-singular rename accepted, want failure")
	}
	if _, err := store.LoadSnapshot("legacy", "zhipu"); err != nil {
		t.Fatalf("legacy snapshot after failed rename: %v", err)
	}
	// The rename never touches other pools' files.
	if _, err := store.LoadSnapshot("other", "zhipu"); err != nil {
		t.Fatalf("unrelated pool: %v", err)
	}
	if _, err := filepath.Abs(store.PoolPath("other")); err != nil {
		t.Fatal(err)
	}
}
