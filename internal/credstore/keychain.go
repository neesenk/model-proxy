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
