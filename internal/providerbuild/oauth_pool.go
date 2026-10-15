// oauth_pool.go — the pooled aqp/codex credential slot: the provider.OAuthBlobStore
// implementation that keeps one OAuth account's credential blob inside its own
// pool entry, plus the per-account guard-secret projection.
package providerbuild

import (
	"encoding/json"
	"fmt"

	"model-proxy/internal/accounts"
	"model-proxy/internal/provider"
)

// poolOAuthStore adapts one OAuth pool account's credential slot to
// provider.OAuthBlobStore.
//
// Load takes the last atomic pool write (no lock — pool files appear whole or
// not at all). An account missing from the pool reads as "not logged in"
// (nil, nil): the account was removed while this instance still held it.
//
// Save runs INSIDE the pool's cross-process lock (load → replace this
// account's blob → save). This is what serializes codex's ROTATING
// refresh_token: two generations' virtuals, a parallel login, and a sibling
// account's refresh can never lose each other's write (the single-file era
// only had an instance-local mutex — a last-writer-wins race that could
// strand a rotated refresh token).
type poolOAuthStore struct {
	store      accounts.Store
	name       string
	providerID string
	id         string
}

func (s poolOAuthStore) Load() ([]byte, error) {
	snapshot, err := s.store.LoadSnapshot(s.name, s.providerID)
	if err != nil {
		return nil, err
	}
	for _, a := range snapshot.Pool.Accounts {
		if a.ID == s.id {
			if len(a.OAuth) == 0 {
				return nil, nil
			}
			return a.OAuth, nil
		}
	}
	return nil, nil
}

func (s poolOAuthStore) Save(blob []byte) error {
	return s.store.WithLock(s.name, func() error {
		snapshot, err := s.store.LoadSnapshot(s.name, s.providerID)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		if snapshot.Source != accounts.SourcePlural {
			return fmt.Errorf("pool %s has no plural account store", s.name)
		}
		for i := range snapshot.Pool.Accounts {
			if snapshot.Pool.Accounts[i].ID != s.id {
				continue
			}
			snapshot.Pool.Accounts[i].Kind = accounts.AccountKindOAuth
			snapshot.Pool.Accounts[i].OAuth = blob
			return s.store.Save(s.name, s.providerID, snapshot.Pool)
		}
		return fmt.Errorf("unknown account %s in pool %s", s.id, s.name)
	})
}

// Delete removes the account from the pool — the logout/delete boundary for
// one pooled OAuth account (owner: accounts.Store.RemoveAccount, including
// keychain cleanup).
func (s poolOAuthStore) Delete() error {
	return s.store.RemoveAccount(s.name, s.providerID, s.id)
}

// oauthAccountSecrets projects one pooled OAuth account's blob into guard
// known-secret values. Parsing mirrors the provider package's own shapes
// (best-effort: an unreadable blob contributes nothing, like the legacy-file
// collector). Labels carry the virtual id — the operator-facing identity
// without credential material.
func oauthAccountSecrets(name, vid, providerID string, blob []byte) []Secret {
	if len(blob) == 0 {
		return nil
	}
	switch providerID {
	case "codex":
		var af provider.CodexAuthFile
		if err := json.Unmarshal(blob, &af); err != nil {
			return nil
		}
		var out []Secret
		for _, tok := range []string{af.Tokens.AccessToken, af.Tokens.RefreshToken, af.Tokens.IDToken} {
			if tok != "" {
				out = append(out, Secret{Value: tok, Label: "oauth:" + vid})
			}
		}
		return out
	case "aqp":
		var a provider.AqpAccountData
		if err := json.Unmarshal(blob, &a); err != nil || a.SSOSessionCookie == "" {
			return nil
		}
		return []Secret{{Value: a.SSOSessionCookie, Label: "oauth:" + vid}}
	}
	return nil
}
