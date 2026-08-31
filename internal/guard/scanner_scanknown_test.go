package guard

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// ScanKnown is the split-exfiltration channel: known-secret exact values and
// their encoded variants ONLY — the embedded rule table, custom patterns and
// sensitive paths must never leak into its report (those are per-request
// signals, already covered by Scan). Fixtures are synthetic.

func TestScanKnown_OnlyKnownSecretChannel(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)

	// Rule-table hit WITHOUT any known secret: Scan fires, ScanKnown must not.
	ruleOnly := "sk-ant-api03-" + strings.Repeat("qW7", 30)
	if got := s.Scan([]byte(ruleOnly)); len(got) == 0 {
		t.Fatal("fixture must hit the embedded rule table via Scan")
	}
	if got := s.ScanKnown([]byte(ruleOnly)); len(got) != 0 {
		t.Errorf("ScanKnown(rule-table-only body) = %v, want no hits", got)
	}

	// Known secret present: ScanKnown reports the type name only.
	body := "token: " + secret
	got := s.ScanKnown([]byte(body))
	if len(got) != 1 || got[0] != knownSecret {
		t.Errorf("ScanKnown(plaintext) = %v, want [%s]", got, knownSecret)
	}
	if strings.Contains(got[0], secret) {
		t.Errorf("ScanKnown report contains secret material")
	}

	// A body hitting BOTH channels reports only the known-secret name.
	both := ruleOnly + " and " + secret
	if got := s.ScanKnown([]byte(both)); len(got) != 1 || got[0] != knownSecret {
		t.Errorf("ScanKnown(both channels) = %v, want [%s] only", got, knownSecret)
	}
}

func TestScanKnown_EncodedVariants(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	forms := map[string]string{
		"base64": base64.StdEncoding.EncodeToString([]byte(secret)),
		"hex":    hex.EncodeToString([]byte(secret)),
	}
	for name, form := range forms {
		got := s.ScanKnown([]byte("blob " + form + " end"))
		if len(got) != 1 || got[0] != knownSecretEncoded {
			t.Errorf("ScanKnown(%s form) = %v, want [%s]", name, got, knownSecretEncoded)
		}
	}
}

// The split-exfiltration core: a secret split into fragments, each clean on
// its own, reassembles only in the concatenation.
func TestScanKnown_FragmentedReassembly(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	half := len(secret) / 2
	frag1, frag2 := secret[:half], secret[half:]
	if got := s.ScanKnown([]byte(frag1)); len(got) != 0 {
		t.Errorf("ScanKnown(fragment 1 alone) = %v, want no hits", got)
	}
	if got := s.ScanKnown([]byte(frag2)); len(got) != 0 {
		t.Errorf("ScanKnown(fragment 2 alone) = %v, want no hits", got)
	}
	// The reassembly point is the junction: the window tail ENDS with fragment
	// 1, the current body STARTS with fragment 2 — the secret is contiguous
	// only in the concatenation.
	joined := `...","content":"` + frag1 + frag2 + `"}...`
	if got := s.ScanKnown([]byte(joined)); len(got) != 1 || got[0] != knownSecret {
		t.Errorf("ScanKnown(concatenation) = %v, want [%s]", got, knownSecret)
	}
}

func TestScanKnown_NoSecretsNeverHits(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	if s.HasKnownSecrets() {
		t.Error("HasKnownSecrets() = true, want false for a secretless scanner")
	}
	if got := s.ScanKnown([]byte("anything at all")); len(got) != 0 {
		t.Errorf("ScanKnown without secrets = %v, want no hits", got)
	}
	s2 := mustScanner(t, nil, []string{syntheticSecret("poolkey-", 32)}, nil)
	if !s2.HasKnownSecrets() {
		t.Error("HasKnownSecrets() = false, want true")
	}
}

// ScanKnownFragment is the cross-request channel: it tracks each known
// secret's longest prefix seen in order and fires only when a later body
// COMPLETES a secret whose earlier fragments arrived in previous requests.

func TestScanKnownFragment_TwoRequestSplit(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	frag1, frag2 := secret[:20], secret[20:]

	fired, progress, reset := s.ScanKnownFragment([]byte(`{"content":"`+frag1+`"}`), nil)
	if fired {
		t.Fatal("first fragment must not fire")
	}
	if len(progress) != 1 || progress[0] != 20 {
		t.Fatalf("progress after frag1 = %v, want [20]", progress)
	}
	if reset != nil {
		t.Errorf("no completion yet: reset = %v, want nil", reset)
	}
	fired, progress, reset = s.ScanKnownFragment([]byte(`{"content":"`+frag2+`"}`), progress)
	if !fired {
		t.Fatal("completing fragment must fire")
	}
	if progress[0] != 0 {
		t.Errorf("progress after completion = %d, want reset to 0", progress[0])
	}
	if len(reset) != 1 || !reset[0] {
		t.Errorf("reset after completion = %v, want [true]", reset)
	}
}

func TestScanKnownFragment_ThreeRequestSplit(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32) // 40 chars
	s := mustScanner(t, nil, []string{secret}, nil)
	parts := []string{secret[:14], secret[14:28], secret[28:]}
	var progress []int
	for i, part := range parts {
		fired, next, _ := s.ScanKnownFragment([]byte("payload "+part), progress)
		progress = next
		if i < len(parts)-1 && fired {
			t.Fatalf("fragment %d must not fire", i+1)
		}
		if i == len(parts)-1 && !fired {
			t.Fatal("final fragment must fire")
		}
	}
}

func TestScanKnownFragment_FullKeyResetsWithoutFiring(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	// Seed progress from an earlier fragment, then send the whole key in one
	// body: the per-request channel owns that signal; no fragmented fire.
	_, progress, _ := s.ScanKnownFragment([]byte(secret[:20]), nil)
	fired, progress, reset := s.ScanKnownFragment([]byte("token "+secret), progress)
	if fired {
		t.Error("a body containing the complete secret must not fire fragmented")
	}
	if progress[0] != 0 {
		t.Errorf("progress after complete key = %d, want 0", progress[0])
	}
	if len(reset) != 1 || !reset[0] {
		t.Errorf("reset after complete key = %v, want [true] (deliberate zeroing)", reset)
	}
}

// Once a secret completed (and the caller honored the returned reset), a
// later body carrying only a suffix fragment must NOT fire again — the
// completion is a terminal state, not a rolling one.
func TestScanKnownFragment_SuffixAfterCompletionDoesNotRefire(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	_, progress, _ := s.ScanKnownFragment([]byte(secret[:20]), nil)
	fired, progress, _ := s.ScanKnownFragment([]byte(secret[20:]), progress)
	if !fired {
		t.Fatal("completing fragment must fire")
	}
	fired, progress, _ = s.ScanKnownFragment([]byte("replay "+secret[20:]), progress)
	if fired {
		t.Error("suffix fragment after an honored completion reset must not re-fire")
	}
	if progress[0] != 0 {
		t.Errorf("suffix fragment after completion advanced progress to %d, want 0", progress[0])
	}
}

func TestScanKnownFragment_OutOfOrderNeverCompletes(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	frag1, frag2 := secret[:20], secret[20:]
	// Suffix first: not a prefix, no progress.
	_, progress, _ := s.ScanKnownFragment([]byte(frag2), nil)
	if progress[0] != 0 {
		t.Fatalf("suffix first: progress = %d, want 0", progress[0])
	}
	// Then the prefix: progress advances but the suffix is gone — no fire.
	fired, _, _ := s.ScanKnownFragment([]byte(frag1), progress)
	if fired {
		t.Error("out-of-order fragments must not fire")
	}
}

func TestScanKnownFragment_ShortFragmentsNotCredited(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	// A 6-byte first fragment is below minKnownFrag: no progress.
	_, progress, _ := s.ScanKnownFragment([]byte(secret[:6]), nil)
	if progress[0] != 0 {
		t.Errorf("sub-minKnownFrag fragment credited: progress = %d", progress[0])
	}
	// A 6-byte completing fragment is below minKnownFrag: no completion.
	_, progress, _ = s.ScanKnownFragment([]byte(secret[:34]), nil)
	fired, _, _ := s.ScanKnownFragment([]byte(secret[34:]), progress)
	if fired {
		t.Error("completing fragment below minKnownFrag must not fire")
	}
}

func TestScanKnownFragment_NoSecrets(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	fired, next, _ := s.ScanKnownFragment([]byte("anything"), nil)
	if fired || next != nil {
		t.Errorf("secretless scanner = (%v, %v), want (false, nil)", fired, next)
	}
}
