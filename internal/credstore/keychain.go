package credstore

import (
	"errors"
	"math"
	"os/exec"
	"runtime"
	"time"

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

// Per-backend entry size ceilings, pre-checked before touching the OS
// keychain so oversized blobs fail fast with a deterministic error class
// instead of pushing a doomed request into the backend (the darwin backend
// shells out to `security -i` with the blob on the command line — a command
// over 4096 bytes is rejected AFTER the process was started, stranding it;
// base64 expansion plus command framing leaves roughly 3000 raw bytes. The
// windows backend caps the credential blob at 2560 raw bytes.).
const (
	maxDarwinEntryBytes  = 3000
	maxWindowsEntryBytes = 2560
)

// maxEntrySize returns this platform's keychain entry ceiling in raw blob
// bytes. Linux/BSD secret service has no practical limit.
func maxEntrySize() int {
	switch runtime.GOOS {
	case "windows":
		return maxWindowsEntryBytes
	case "darwin":
		return maxDarwinEntryBytes
	default:
		return math.MaxInt
	}
}

func mapKeyringErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, keyring.ErrSetDataTooBig):
		return ErrEntryTooLarge
	case errors.Is(err, keyring.ErrUnsupportedPlatform):
		return ErrUnavailable
	default:
		// Wrap without echoing any backend payload — only the error class.
		return ErrUnavailable
	}
}

// keychainOpTimeout bounds one OS keychain operation. The backends shell out
// (/usr/bin/security) or speak dbus with no context support, so a wedged
// secret service would otherwise hang a credential load forever — including
// the first request a provider serves (loads cache in memory after that) and
// OAuth refresh beats. Generous enough for a human to unlock a locked
// keychain through the GUI prompt; short enough that a dead backend fails
// closed as ErrUnavailable instead of hanging the caller. The underlying
// syscall is not cancellable: on timeout the goroutine is abandoned (bounded
// — one per timed-out call, result discarded), the standard Go tradeoff for
// uncancellable I/O.
var keychainOpTimeout = 30 * time.Second

func withKeychainTimeout(op func() error) error {
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		return err
	case <-time.After(keychainOpTimeout):
		return ErrUnavailable
	}
}

func withKeychainTimeoutGet(op func() ([]byte, error)) ([]byte, error) {
	type result struct {
		blob []byte
		err  error
	}
	done := make(chan result, 1)
	go func() { blob, err := op(); done <- result{blob, err} }()
	select {
	case r := <-done:
		return r.blob, r.err
	case <-time.After(keychainOpTimeout):
		return nil, ErrUnavailable
	}
}

func withKeychainTimeoutBool(op func() bool) bool {
	done := make(chan bool, 1)
	go func() { done <- op() }()
	select {
	case ok := <-done:
		return ok
	case <-time.After(keychainOpTimeout):
		return false
	}
}

func (realKeychainProvider) Set(service, user string, password []byte) error {
	if len(password) > maxEntrySize() {
		return ErrEntryTooLarge
	}
	return withKeychainTimeout(func() error {
		return mapKeyringErr(keyring.Set(service, user, string(password)))
	})
}

func (realKeychainProvider) Get(service, user string) ([]byte, error) {
	return withKeychainTimeoutGet(func() ([]byte, error) {
		s, err := keyring.Get(service, user)
		if err != nil {
			return nil, mapKeyringErr(err)
		}
		return []byte(s), nil
	})
}

func (realKeychainProvider) Delete(service, user string) error {
	return withKeychainTimeout(func() error {
		return mapKeyringErr(keyring.Delete(service, user))
	})
}

func (realKeychainProvider) Available(service string) bool {
	return withKeychainTimeoutBool(func() bool {
		_, err := keyring.Get(service, probeAccount)
		if err == nil {
			// Unexpected entry under the probe name: reachable either way.
			_ = keyring.Delete(service, probeAccount)
			return true
		}
		return errors.Is(err, keyring.ErrNotFound)
	})
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
