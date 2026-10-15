package login

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/accounts"
	"model-proxy/internal/provider"
)

// oauthTestHome isolates HOME and returns the legacy OAuth file path helper.
func oauthTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func writeLegacyAqpFile(t *testing.T, home, name string, a provider.AqpAccountData) {
	t.Helper()
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveAqpAccount(accounts.AuthFilePath(name, "oauth_auth"), &a); err != nil {
		t.Fatal(err)
	}
}

// TestAddOAuthAccount_AbsorbsLegacyAsFirstEntry pins the first-write contract:
// adding a SECOND aqp account absorbs the legacy single-account file as pool
// entry #1 (identity anchored on its upstream email), so the historical
// account keeps serving without a manual migration step.
func TestAddOAuthAccount_AbsorbsLegacyAsFirstEntry(t *testing.T) {
	home := oauthTestHome(t)
	writeLegacyAqpFile(t, home, "aqp", provider.AqpAccountData{
		AccountID:        "old@x.com",
		Email:            "old@x.com",
		ProjectID:        "proj",
		SSOSessionCookie: "SSO_C=old",
	})

	newSeed := OAuthAccountSeed{
		UpstreamID: "new@x.com",
		Email:      "new@x.com",
		Blob:       []byte(`{"account_id":"new@x.com","sso_session_cookie":"SSO_C=new"}`),
	}
	id, err := AddOAuthAccount("aqp", "aqp", newSeed)
	if err != nil {
		t.Fatal(err)
	}
	if id != accounts.OAuthAccountID("new@x.com") {
		t.Fatalf("returned id = %q", id)
	}

	pool, err := accountStoreEnv().Load("aqp", "aqp")
	if err != nil || len(pool.Accounts) != 2 {
		t.Fatalf("pool = %+v, %v (want legacy + new)", pool, err)
	}
	byID := map[string]accounts.Account{}
	for _, a := range pool.Accounts {
		byID[a.ID] = a
	}
	legacy := byID[accounts.OAuthAccountID("old@x.com")]
	if !accounts.IsOAuthAccount(legacy) || legacy.Email != "old@x.com" {
		t.Fatalf("absorbed legacy entry = %+v", legacy)
	}
	var data provider.AqpAccountData
	if err := json.Unmarshal(legacy.OAuth, &data); err != nil || data.SSOSessionCookie != "SSO_C=old" || data.ProjectID != "proj" {
		t.Fatalf("absorbed legacy blob = %s (%v)", legacy.OAuth, err)
	}
	if _, ok := byID[id]; !ok {
		t.Fatalf("new account missing: %+v", pool.Accounts)
	}
}

// TestAddOAuthAccount_SilentReplaceKeepsIdentity pins the re-authorization
// contract: logging in again with the SAME upstream identity replaces only the
// credential blob — label and added_at survive, so usage-history attribution
// stays continuous.
func TestAddOAuthAccount_SilentReplaceKeepsIdentity(t *testing.T) {
	oauthTestHome(t)
	first := OAuthAccountSeed{
		UpstreamID: "one@x.com",
		Email:      "one@x.com",
		Blob:       []byte(`{"account_id":"one@x.com","sso_session_cookie":"SSO_C=v1"}`),
	}
	id, err := AddOAuthAccount("aqp", "aqp", first)
	if err != nil {
		t.Fatal(err)
	}
	// User renames the account, then re-authorizes it.
	if err := accountStoreEnv().SetLabel("aqp", "aqp", id, "work horse"); err != nil {
		t.Fatal(err)
	}
	reseed := OAuthAccountSeed{
		UpstreamID: "one@x.com",
		Email:      "one@x.com",
		Blob:       []byte(`{"account_id":"one@x.com","sso_session_cookie":"SSO_C=v2"}`),
	}
	id2, err := AddOAuthAccount("aqp", "aqp", reseed)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Fatalf("re-authorization drifted the id: %q → %q", id, id2)
	}
	pool, err := accountStoreEnv().Load("aqp", "aqp")
	if err != nil || len(pool.Accounts) != 1 {
		t.Fatalf("pool = %+v, %v (replace must not append)", pool, err)
	}
	entry := pool.Accounts[0]
	if entry.Label != "work horse" {
		t.Fatalf("label lost on re-authorization: %+v", entry)
	}
	var data provider.AqpAccountData
	if err := json.Unmarshal(entry.OAuth, &data); err != nil || data.SSOSessionCookie != "SSO_C=v2" {
		t.Fatalf("blob must be the fresh credential: %s (%v)", entry.OAuth, err)
	}
}

// TestCodexOAuthSeed_AnchorsOnAccountID pins the codex id derivation: the
// chatgpt_account_id from the id_token drives the anchored id; the email claim
// lands as display metadata.
func TestCodexOAuthSeed_AnchorsOnAccountID(t *testing.T) {
	af := &provider.CodexAuthFile{}
	af.Tokens.AccountID = "acct-9"
	af.Tokens.AccessToken = "at"
	af.Tokens.RefreshToken = "rt"
	seed, err := CodexOAuthSeed(af)
	if err != nil {
		t.Fatal(err)
	}
	if seed.UpstreamID != "acct-9" {
		t.Fatalf("upstream id = %q", seed.UpstreamID)
	}
	if _, err := CodexOAuthSeed(&provider.CodexAuthFile{}); err == nil {
		t.Fatal("identity-less codex login must be rejected")
	}
}
