package credstore

import (
	"errors"
	"os/exec"
	"runtime"

	"github.com/zalando/go-keyring"
)

// keychainServiceProvider abstracts the OS keychain backend. The real
// implementation wraps zalando/go-keyring (pure Go on every supported OS: the
// macOS backend shells out to /usr/bin/security, so CGO_ENABLED=0 static
// builds stay intact). Tests inject fakes via the keychainOps var (in-package).
type keychainServiceProvider interface {
	Set(service, user string, password []byte) error
	Get(service, user string) ([]byte, error)
	Delete(service, user string) error
	// Available probes reachability WITHOUT creating entries or reading real
	// credentials: a NotFound answer proves the backend responds.
	Available(service string) bool
}

// keychainOps is the injection seam for tests; production always uses
// realKeychainProvider.
var keychainOps keychainServiceProvider = realKeychainProvider{}

type realKeychainProvider struct{}

func mapKeyringErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, keyring.ErrUnsupportedPlatform):
		return ErrUnavailable
	default:
		// Wrap without echoing any backend payload — only the error class.
		return ErrUnavailable
	}
}

func (realKeychainProvider) Set(service, user string, password []byte) error {
	return mapKeyringErr(keyring.Set(service, user, string(password)))
}

func (realKeychainProvider) Get(service, user string) ([]byte, error) {
	s, err := keyring.Get(service, user)
	if err != nil {
		return nil, mapKeyringErr(err)
	}
	return []byte(s), nil
}

func (realKeychainProvider) Delete(service, user string) error {
	return mapKeyringErr(keyring.Delete(service, user))
}

func (realKeychainProvider) Available(service string) bool {
	_, err := keyring.Get(service, probeAccount)
	if err == nil {
		// Unexpected entry under the probe name: reachable either way.
		_ = keyring.Delete(service, probeAccount)
		return true
	}
	return errors.Is(err, keyring.ErrNotFound)
}

// keychainAvailable reports whether auto mode may select the keychain.
func keychainAvailable() bool {
	if runtime.GOOS == "darwin" {
		// The darwin backend shells out to /usr/bin/security; a missing binary
		// means unreachable without ever invoking the probe.
		if _, err := exec.LookPath("security"); err != nil {
			return false
		}
	}
	return keychainOps.Available(serviceName)
}

// Entry-level keychain access for callers that keep their own metadata files
// and store only secret VALUES in the keychain (the accounts apikey pools
// under config `credentials: keychain`: pool file holds id/label/added_at,
// api_key/access_key/secret_key live here under caller-chosen keys). Unlike
// Ref, these never consult ResolvedMode and never touch the filesystem — the
// caller selects the backend explicitly and treats ErrUnavailable/ErrNotFound
// as fail-closed. Key material is never echoed into errors (mapKeyringErr
// keeps only the error class).
func KeychainGet(key string) (string, error) {
	blob, err := keychainOps.Get(serviceName, key)
	if err != nil {
		return "", err
	}
	return string(blob), nil
}

// KeychainSet stores one secret value under key (see KeychainGet).
func KeychainSet(key, value string) error {
	return keychainOps.Set(serviceName, key, []byte(value))
}

// KeychainDelete removes one entry (see KeychainGet). ErrNotFound means the
// entry was already absent.
func KeychainDelete(key string) error {
	return keychainOps.Delete(serviceName, key)
}
