package credstore

import (
	"errors"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// The realKeychainProvider tests use go-keyring's own in-memory mock backend
// (keyring.MockInit / MockInitWithError) — no OS keychain is ever touched.
// Mode resolution stays in file mode throughout; these tests call the provider
// methods directly.

func withMockedKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Cleanup(keyring.MockInit) // re-init to a fresh store for the next test
}

func TestRealKeychainProviderRoundTrip(t *testing.T) {
	withMockedKeyring(t)
	p := realKeychainProvider{}

	if err := p.Set(serviceName, "acct", []byte("secret")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := p.Get(serviceName, "acct")
	if err != nil || string(got) != "secret" {
		t.Fatalf("Get after Set = (%q, %v)", got, err)
	}
	if !p.Available(serviceName) {
		t.Fatal("mock-backed keychain must report Available (probe entry NotFound)")
	}
	if err := p.Delete(serviceName, "acct"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.Get(serviceName, "acct"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := p.Delete(serviceName, "acct"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idempotent Delete = %v, want ErrNotFound", err)
	}
}

func TestMapKeyringErr(t *testing.T) {
	cases := []struct {
		in   error
		want error
	}{
		{nil, nil},
		{keyring.ErrNotFound, ErrNotFound},
		{keyring.ErrUnsupportedPlatform, ErrUnavailable},
		{errors.New("dbus: connection refused"), ErrUnavailable},
	}
	for _, tc := range cases {
		if got := mapKeyringErr(tc.in); !errors.Is(got, tc.want) && got != tc.want {
			t.Errorf("mapKeyringErr(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// ErrNotFound must NOT be swallowed into ErrUnavailable.
	if got := mapKeyringErr(keyring.ErrNotFound); got != ErrNotFound {
		t.Errorf("mapKeyringErr(ErrNotFound) = %v, want identity ErrNotFound", got)
	}
}

func TestRealKeychainProviderErrorMapping(t *testing.T) {
	keyring.MockInitWithError(errors.New("backend down"))
	defer keyring.MockInit()
	p := realKeychainProvider{}

	if err := p.Set(serviceName, "a", []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set on failing backend = %v, want ErrUnavailable", err)
	}
	if _, err := p.Get(serviceName, "a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get on failing backend = %v, want ErrUnavailable", err)
	}
	if err := p.Delete(serviceName, "a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete on failing backend = %v, want ErrUnavailable", err)
	}
	if p.Available(serviceName) {
		t.Fatal("failing backend must not report Available")
	}
}

// TestKeychainAvailableFollowsBackend pins the auto-mode probe decision.
func TestKeychainAvailableFollowsBackend(t *testing.T) {
	withMockedKeyring(t)
	if !keychainAvailable() {
		t.Fatal("reachable mock backend must report keychainAvailable")
	}
	keyring.MockInitWithError(errors.New("dbus down"))
	t.Cleanup(keyring.MockInit)
	if keychainAvailable() {
		t.Fatal("failing backend must not report keychainAvailable")
	}
}

// TestAutoModeNeverProbesRealKeychainInTests pins the hermeticity guard:
// EffectiveMode under testing.Testing() resolves to file mode without
// touching the probe — even when a mock keychain would answer Available()==true.
func TestAutoModeNeverProbesRealKeychainInTests(t *testing.T) {
	resetResolution()
	t.Cleanup(resetResolution)
	osUnsetenvCredStore(t)

	if ResolvedMode() != ModeFile {
		t.Fatalf("auto mode under tests resolved to %q, want file (hermeticity guard)", ResolvedMode())
	}
}

// TestExplicitKeychainModeResolvable pins that explicit env selection bypasses
// the test-binary guard (needed by the fake-ops migration tests).
func TestExplicitKeychainModeResolvable(t *testing.T) {
	useFakeKeychain(t, newFakeKeychain(true))
	if ResolvedMode() != ModeKeychain {
		t.Fatalf("explicit keychain env resolved to %q, want keychain", ResolvedMode())
	}
}

// The timeout wrapper bounds one uncancellable keychain backend call: a
// wedged secret service must surface as ErrUnavailable, not hang the caller.
// The abandoned op goroutine is still joined by the test (no leak).
func TestKeychainTimeoutWrapper(t *testing.T) {
	orig := keychainOpTimeout
	keychainOpTimeout = 10 * time.Millisecond
	t.Cleanup(func() { keychainOpTimeout = orig })

	release := make(chan struct{})
	opReturned := make(chan struct{})
	if err := withKeychainTimeout(func() error {
		defer close(opReturned)
		<-release
		return nil
	}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("wedged op = %v, want ErrUnavailable after timeout", err)
	}
	close(release)
	select {
	case <-opReturned:
	case <-time.After(time.Second):
		t.Fatal("op goroutine did not exit after release")
	}

	// A prompt op's result (including backend error classes) passes through
	// unchanged.
	sentinel := errors.New("backend class")
	if err := withKeychainTimeout(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("prompt op error = %v, want pass-through", err)
	}
	blob, err := withKeychainTimeoutGet(func() ([]byte, error) { return []byte("v"), nil })
	if err != nil || string(blob) != "v" {
		t.Errorf("prompt get = (%q, %v), want pass-through", blob, err)
	}
	if !withKeychainTimeoutBool(func() bool { return true }) {
		t.Error("prompt bool = false, want true")
	}
}
