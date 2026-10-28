package accounts

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxLabelLen bounds an account label. Labels are display-only identity, but
// they surface in logs, the Web UI and CLI output — an unbounded string lets
// one rename flood every later view.
const MaxLabelLen = 64

// SetLabel renames the display label of the account with the given id under
// the cross-process pool lock (re-read → mutate → save, like every other
// mutator). The label is metadata only: it never takes part in the account ID,
// the virtual runtime key, or any credential derivation, so a rename needs no
// reload and never re-keys usage history. Fail-closed on an unknown id or an
// invalid label — the pool file is left untouched.
func (s Store) SetLabel(name, providerID, id, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return fmt.Errorf("label is empty")
	}
	if utf8.RuneCountInString(label) > MaxLabelLen {
		return fmt.Errorf("label is longer than %d characters", MaxLabelLen)
	}
	if strings.TrimSpace(id) == "" || strings.Contains(id, "#") {
		return fmt.Errorf("invalid account id")
	}
	return s.WithLock(name, func() error {
		snapshot, err := s.LoadSnapshot(name, providerID)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		if snapshot.Source != SourcePlural {
			return fmt.Errorf("pool %s has no plural account store", name)
		}
		for i := range snapshot.Pool.Accounts {
			if snapshot.Pool.Accounts[i].ID == id {
				snapshot.Pool.Accounts[i].Label = label
				return s.Save(name, providerID, snapshot.Pool)
			}
		}
		return fmt.Errorf("unknown account %s in pool %s", id, name)
	})
}
