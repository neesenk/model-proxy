package runtime

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The forwarding path proves some credential failures the quota poll never
// sees: step-plan's Quota() always succeeds, so a lapsed plan subscription or a
// revoked key leaves no sync-error record for the credential banner. Those
// account attributions land here instead — one record per account, shown by
// the operator surfaces until the account's next success.

func newCredentialDenialManager(generation uint64, at time.Time) (*Manager, *time.Time) {
	t := &at
	m := newTestManager(generation)
	m.now = func() time.Time { return *t }
	return m, t
}

func TestRecordCredentialDenialSurfacesInDashboard(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 10, 34, 0, 0, time.UTC)
	m, _ := newCredentialDenialManager(7, base)

	m.RecordCredentialDenial("step-plan#acct", "plan-inactive", "you have no active step plan subscription", 7)

	got := m.Dashboard(base).CredentialDenials["step-plan#acct"]
	if got.Class != "plan-inactive" || got.Err != "you have no active step plan subscription" || !got.Since.Equal(base) {
		t.Fatalf("denial = %+v, want the plan-inactive record stamped at the observation time", got)
	}
	// Detached copy: a caller mutating the snapshot must not touch live state.
	snap := m.Dashboard(base)
	snap.CredentialDenials["step-plan#acct"] = CredentialDenial{Class: "tampered"}
	if again := m.Dashboard(base).CredentialDenials["step-plan#acct"].Class; again != "plan-inactive" {
		t.Fatalf("class after mutating the snapshot = %q, want the live record untouched", again)
	}
}

func TestRecordCredentialDenialRepeatedKeepsFirstObservation(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 10, 34, 0, 0, time.UTC)
	m, clock := newCredentialDenialManager(7, base)
	const key = "zcode#acct"

	m.RecordCredentialDenial(key, "auth", "first", 7)
	*clock = base.Add(time.Minute)
	m.RecordCredentialDenial(key, "auth", "second", 7)
	got := m.Dashboard(*clock).CredentialDenials[key]
	if !got.Since.Equal(base) {
		t.Fatalf("Since = %v, want the episode's first observation %v (the banner says how long the account has been dead)", got.Since, base)
	}
	if got.Err != "second" {
		t.Fatalf("Err = %q, want the latest observation's evidence", got.Err)
	}
	// A different class is a different fact about the same dead episode: the
	// class follows the latest observation, the start does not move.
	*clock = base.Add(2 * time.Minute)
	m.RecordCredentialDenial(key, "plan-inactive", "third", 7)
	if got = m.Dashboard(*clock).CredentialDenials[key]; got.Class != "plan-inactive" || !got.Since.Equal(base) {
		t.Fatalf("denial after re-classification = %+v, want class plan-inactive keeping Since %v", got, base)
	}
}

func TestRecordSuccessClearsOnlyItsOwnCredentialDenial(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 10, 34, 0, 0, time.UTC)
	m, _ := newCredentialDenialManager(7, base)
	m.RecordCredentialDenial("pool#a", "auth", "a", 7)
	m.RecordCredentialDenial("pool#b", "auth", "b", 7)

	m.RecordSuccess("pool#a", "model", 7)

	denials := m.Dashboard(base).CredentialDenials
	if _, alive := denials["pool#a"]; alive {
		t.Fatalf("pool#a denial survived its own success: %+v", denials)
	}
	if denials["pool#b"].Class != "auth" {
		t.Fatalf("pool#b denial = %+v, want untouched by a sibling's success", denials["pool#b"])
	}
}

func TestReplaceGenerationPrunesCredentialDenialsToLiveKeys(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 10, 34, 0, 0, time.UTC)
	m, _ := newCredentialDenialManager(7, base)
	m.RecordCredentialDenial("pool#stays", "auth", "stays", 7)
	m.RecordCredentialDenial("pool#removed", "auth", "removed", 7)

	m.ReplaceGeneration(8, map[string]bool{"pool#stays": true})

	denials := m.Dashboard(base).CredentialDenials
	if denials["pool#stays"].Class != "auth" {
		t.Fatalf("denial for the surviving account = %+v, want kept across the reload (health is config-coupled, this is not)", denials["pool#stays"])
	}
	if _, alive := denials["pool#removed"]; alive {
		t.Fatalf("denial for the removed account survived the reload: %+v", denials["pool#removed"])
	}
}

func TestResetHealthKeepsCredentialDenial(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 10, 34, 0, 0, time.UTC)
	m, _ := newCredentialDenialManager(7, base)
	m.RecordCredentialDenial("pool#a", "plan-inactive", "lapsed", 7)

	// unfreeze is the operator asking to retry NOW; it is not the credential
	// recovering, so the banner must not blink off.
	m.ResetHealth("", nil)

	if got := m.Dashboard(base).CredentialDenials["pool#a"].Class; got != "plan-inactive" {
		t.Fatalf("denial after unfreeze = %q, want plan-inactive kept", got)
	}
}

func TestRecordCredentialDenialRejectsStaleGenerationAndEmptyClass(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 10, 34, 0, 0, time.UTC)
	m, _ := newCredentialDenialManager(7, base)

	m.RecordCredentialDenial("pool#stale", "auth", "x", 6) // a pre-reload in-flight request
	m.RecordCredentialDenial("pool#noclass", "", "x", 7)

	if denials := m.Dashboard(base).CredentialDenials; len(denials) != 0 {
		t.Fatalf("denials = %+v, want none (stale generation, empty class)", denials)
	}
}

func TestRecordCredentialDenialBoundsEvidence(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 11, 25, 10, 34, 0, 0, time.UTC)
	long := strings.Repeat("é", credentialDenialErrMaxRunes*3)
	cases := []struct {
		name, err, want string
	}{
		{name: "first line only", err: "{\"error\":{\"message\":\"no active step plan subscription\"}}\n<html>trailing page</html>", want: "{\"error\":{\"message\":\"no active step plan subscription\"}}"},
		{name: "rune-bounded", err: long, want: strings.Repeat("é", credentialDenialErrMaxRunes)},
		{name: "trimmed", err: "  Invalid API key \n", want: "Invalid API key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newCredentialDenialManager(7, base)
			m.RecordCredentialDenial("pool#a", "auth", tc.err, 7)
			got := m.Dashboard(base).CredentialDenials["pool#a"].Err
			if got != tc.want {
				t.Fatalf("Err = %q (%d runes), want %q", got, utf8.RuneCountInString(got), tc.want)
			}
		})
	}
}
