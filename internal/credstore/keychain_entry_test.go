package credstore

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// Entry-level API (KeychainGet/Set/Delete) coverage: same in-memory mock as
// the provider tests — no OS keychain, no ResolvedMode consultation.

func TestKeychainEntryRoundTrip(t *testing.T) {
	withMockedKeyring(t)

	if err := KeychainSet("prov/abc/api_key", "secret"); err != nil {
		t.Fatalf("KeychainSet: %v", err)
	}
	got, err := KeychainGet("prov/abc/api_key")
	if err != nil || got != "secret" {
		t.Fatalf("KeychainGet after Set = (%q, %v)", got, err)
	}
	if err := KeychainDelete("prov/abc/api_key"); err != nil {
		t.Fatalf("KeychainDelete: %v", err)
	}
	if _, err := KeychainGet("prov/abc/api_key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("KeychainGet after Delete = %v, want ErrNotFound", err)
	}
}

func TestKeychainEntryErrorMapping(t *testing.T) {
	keyring.MockInitWithError(errors.New("backend down"))
	defer keyring.MockInit()

	if err := KeychainSet("k", "v"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("KeychainSet on failing backend = %v, want ErrUnavailable", err)
	}
	if _, err := KeychainGet("k"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("KeychainGet on failing backend = %v, want ErrUnavailable", err)
	}
	if err := KeychainDelete("k"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("KeychainDelete on failing backend = %v, want ErrUnavailable", err)
	}
}
