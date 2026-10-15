package login

// oauth_pool.go — the OAuth (aqp/codex) account upsert: the pool counterpart
// of AddApikeyAccount. Login flows (web async jobs, CLI, --from-codex import)
// complete with the upstream credentials in hand and land them here.

import (
	"encoding/json"
	"fmt"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/provider"
)

// OAuthAccountSeed is one completed login's account material, shaped by the
// provider-specific login flow (aqp SSO / codex device flow).
type OAuthAccountSeed struct {
	// UpstreamID is the upstream account identity the anchored pool id is
	// derived from (aqp employee email, codex chatgpt_account_id). Callers
	// fall back to a stable credential hash when the upstream reports none.
	UpstreamID string
	Email      string
	Label      string
	Blob       []byte
}

// AddOAuthAccount upserts one OAuth account into the provider's pool under the
// cross-process lock. Semantics (the OAuth contract differs from API-key add
// on purpose): a completed SSO/device flow IS the user's confirmation, so an
// existing account with the same upstream identity is SILENTLY REPLACED
// (re-authorization refreshes its credential; id, label and added_at survive —
// usage history attribution stays continuous). When the pool is created for
// the first time, the legacy single-account <name>_oauth_auth.json is absorbed
// as entry #1 (plural becomes authoritative, same as the API-key pools).
// Returns the anchored account id.
func AddOAuthAccount(name, providerID string, seed OAuthAccountSeed) (string, error) {
	if len(seed.Blob) == 0 {
		return "", fmt.Errorf("empty oauth credential blob")
	}
	if seed.UpstreamID == "" {
		return "", fmt.Errorf("empty upstream account id")
	}
	id := accounts.OAuthAccountID(seed.UpstreamID)
	err := withPoolLock(name, func() error {
		snapshot, err := LoadPoolSnapshotForMutation(name, providerID)
		if err != nil {
			return err
		}
		pool := snapshot.Pool
		if snapshot.Source != accounts.SourcePlural {
			// First pool write: absorb the legacy single-account file so the
			// historical account keeps serving (and keeps its history).
			pool = accounts.Pool{Version: 1}
			if legacy := absorbLegacyOAuthAccount(name, providerID); legacy != nil && legacy.ID != id {
				pool.Accounts = append(pool.Accounts, *legacy)
				logx.Infof("[accounts] absorbed the legacy %s oauth store into pool %s as its first account", providerID, name)
			}
		}
		now := nowTS()
		entry := accounts.Account{
			ID:      id,
			Kind:    accounts.AccountKindOAuth,
			Label:   seed.Label,
			Email:   seed.Email,
			OAuth:   seed.Blob,
			AddedAt: now,
		}
		replaced := false
		for i := range pool.Accounts {
			if pool.Accounts[i].ID != id {
				continue
			}
			// Re-authorization: swap the credential, keep the identity
			// (label/added_at — the account's history attribution).
			keep := pool.Accounts[i]
			entry.Label = firstNonEmpty(entry.Label, keep.Label)
			entry.AddedAt = keep.AddedAt
			pool.Accounts[i] = entry
			replaced = true
			break
		}
		if !replaced {
			if entry.Label == "" {
				entry.Label = firstNonEmpty(entry.Email, id)
			}
			pool.Accounts = append(pool.Accounts, entry)
		}
		return savePool(name, providerID, pool)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// LoadPoolSnapshotForMutation reads the pool inside the mutation lock. An
// unreadable plural file is an error (never downgraded); a missing pool
// (SourceMissing) is the legitimate first-write state.
func LoadPoolSnapshotForMutation(name, providerID string) (accounts.Snapshot, error) {
	snapshot, err := accountStoreEnv().LoadSnapshot(name, providerID)
	if err != nil {
		return snapshot, fmt.Errorf("load pool: %w", err)
	}
	return snapshot, nil
}

// absorbLegacyOAuthAccount reads the legacy single-account OAuth file
// (<name>_oauth_auth.json) and projects it into a pool account entry. nil when
// the file is absent/unreadable/shapeless — nothing to absorb. Provider-aware:
// the id anchor and email come from each provider's own account identity.
func absorbLegacyOAuthAccount(name, providerID string) *accounts.Account {
	switch providerID {
	case "aqp":
		a, err := provider.LoadAqpAccount(accounts.AuthFilePath(name, "oauth_auth"))
		if err != nil || a == nil || a.SSOSessionCookie == "" || a.AccountID == "" {
			return nil
		}
		blob, err := json.Marshal(a)
		if err != nil {
			return nil
		}
		added := a.CreatedAt
		if added == 0 {
			added = time.Now().UTC().Unix()
		}
		return &accounts.Account{
			ID:      accounts.OAuthAccountID(a.AccountID),
			Kind:    accounts.AccountKindOAuth,
			Label:   firstNonEmpty(a.Email, accounts.OAuthAccountID(a.AccountID)),
			Email:   a.Email,
			OAuth:   blob,
			AddedAt: time.Unix(added, 0).UTC().Format(time.RFC3339),
		}
	case "codex":
		af, err := provider.LoadCodexAuthFile(accounts.AuthFilePath(name, "oauth_auth"))
		if err != nil || af == nil {
			return nil
		}
		if af.Tokens.AccessToken == "" && af.Tokens.RefreshToken == "" {
			return nil
		}
		blob, err := json.Marshal(af)
		if err != nil {
			return nil
		}
		upstream := provider.AccountIDFromTokens(af.Tokens.IDToken, af.Tokens.AccountID)
		if upstream == "" {
			// No upstream identity discoverable: anchor on the refresh token
			// hash — stable across access-token rotation (same "hash, never
			// raw" rule as every other id derivation).
			upstream = af.Tokens.RefreshToken
		}
		if upstream == "" {
			return nil
		}
		id := accounts.OAuthAccountID(upstream)
		email := provider.EmailFromIDToken(af.Tokens.IDToken)
		return &accounts.Account{
			ID:      id,
			Kind:    accounts.AccountKindOAuth,
			Label:   firstNonEmpty(email, id),
			Email:   email,
			OAuth:   blob,
			AddedAt: nowTS(),
		}
	}
	return nil
}

// CodexOAuthSeed projects a completed codex device-flow exchange into an
// OAuthAccountSeed (blob + upstream account identity + email).
func CodexOAuthSeed(af *provider.CodexAuthFile) (OAuthAccountSeed, error) {
	blob, err := json.Marshal(af)
	if err != nil {
		return OAuthAccountSeed{}, err
	}
	upstream := provider.AccountIDFromTokens(af.Tokens.IDToken, af.Tokens.AccountID)
	if upstream == "" {
		// Not in the tokens (unparsable id_token): anchor on the refresh
		// token hash so the account still pools deterministically.
		upstream = af.Tokens.RefreshToken
	}
	if upstream == "" {
		return OAuthAccountSeed{}, fmt.Errorf("codex login returned no account identity")
	}
	return OAuthAccountSeed{
		UpstreamID: upstream,
		Email:      provider.EmailFromIDToken(af.Tokens.IDToken),
		Blob:       blob,
	}, nil
}

// AqpOAuthSeed projects a completed aqp SSO login into an OAuthAccountSeed.
// The stored AqpAccountData doubles as the blob.
func AqpOAuthSeed(a *provider.AqpAccountData) (OAuthAccountSeed, error) {
	blob, err := json.Marshal(a)
	if err != nil {
		return OAuthAccountSeed{}, err
	}
	if a.AccountID == "" {
		return OAuthAccountSeed{}, fmt.Errorf("aqp login returned no account identity")
	}
	return OAuthAccountSeed{
		UpstreamID: a.AccountID,
		Email:      a.Email,
		Blob:       blob,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
