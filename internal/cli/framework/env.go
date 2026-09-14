package framework

import (
	"sync"

	"model-proxy/internal/accounts"
)

// env.go holds the environment seams shared by the CLI command packages
// (account, doctor, models, ...): the lazily-resolved credential-pool store.
// The home directory seam is HomeDir in args.go.

// LazyAccountStore caches the credential-pool store keyed by the current home
// directory, so tests can isolate HOME (t.Setenv) before first use — a
// package-init-time store would pin the real home directory.
type LazyAccountStore struct {
	mu  sync.Mutex
	key string
	val accounts.Store
}

// Get returns the store for the current home directory, rebuilding it when
// HOME changed since the last call.
func (s *LazyAccountStore) Get() accounts.Store {
	home := HomeDir()
	s.mu.Lock()
	defer s.mu.Unlock()
	if home != s.key {
		s.val = accounts.NewStore(home)
		s.key = home
	}
	return s.val
}
