package provider

import (
	"errors"

	"model-proxy/internal/credstore"
)

// OAuthBlobStore is the per-account OAuth credential seam: Load returns the
// account's credential blob (the provider's own JSON shape — AqpAccountData /
// CodexAuthFile), Save persists it atomically, Delete removes it. The default
// implementation is file-backed (the historical single-account
// <name>_oauth_auth.json via credstore); pooled accounts bind a pool-slot
// implementation (see providerbuild) so a token refresh writes back into the
// account's OWN pool entry under the pool's cross-process lock instead of
// clobbering sibling accounts or racing a parallel login.
//
// Load's contract: (nil, nil) means "no account stored" (never logged in /
// removed); every other error is a real store failure.
type OAuthBlobStore interface {
	Load() ([]byte, error)
	Save(blob []byte) error
	Delete() error
}

// FileOAuthStore adapts one credstore ref path to OAuthBlobStore — the
// historical single-account behavior, file and keychain modes included.
type FileOAuthStore struct{ Path string }

func (f FileOAuthStore) Load() ([]byte, error) {
	if f.Path == "" {
		return nil, nil
	}
	data, err := credstore.NewRef(f.Path).Load()
	if err != nil {
		if errors.Is(err, credstore.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

func (f FileOAuthStore) Save(blob []byte) error {
	return credstore.NewRef(f.Path).Save(blob)
}

func (f FileOAuthStore) Delete() error {
	return credstore.NewRef(f.Path).Delete()
}

// oauthStoreOrDefault resolves the effective blob store for one aqp/codex
// instance: the injected seam when the provider was built from a pool slot,
// else the legacy single-account file path.
func oauthStoreOrDefault(store OAuthBlobStore, authFile string) OAuthBlobStore {
	if store != nil {
		return store
	}
	return FileOAuthStore{Path: authFile}
}
