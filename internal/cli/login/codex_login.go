package login

import (
	"fmt"
	"os"
	"strconv"

	logincore "model-proxy/internal/login"
	"model-proxy/internal/provider"
)

// runCodexLoginFlow performs the interactive device flow and persists tokens
// to authFile. Errors are returned (no process exit), so non-CLI orchestrators
// (the `add` command) can drive the same flow. The caller derives authFile from
// the config top-level key (NOT the provider_id "codex") via oauthAuthFilePath,
// so credentials land in <provName>_oauth_auth.json — the same path the forward
// path / web UI / logout read (a renamed instance like "codex-work" writes
// codex-work_oauth_auth.json).
//
// This is the CLI wrapper: it owns stdout progress printing around the
// transport-neutral device-flow steps in internal/login (RequestUserCode →
// PollForToken → ExchangeCodeForTokens).
func runCodexLoginFlow(authFile string) error {
	opts := &logincore.CodexLoginServerOptions{}
	opts.Defaults()

	fmt.Println("Requesting device code from OpenAI...")
	uc, err := logincore.RequestUserCode(opts, provider.CodexOAuthClientID)
	if err != nil {
		return err
	}
	fmt.Printf("\n  Open this URL: %s\n", logincore.CodexOAuthVerifyURL)
	fmt.Printf("  Enter code:   %s\n\n", uc.UserCode)
	fmt.Println("Waiting for authorization (15 min timeout)...")

	interval, _ := strconv.Atoi(uc.Interval)
	cs, err := logincore.PollForToken(opts, uc.DeviceAuthID, uc.UserCode, interval)
	if err != nil {
		return err
	}
	fmt.Println("Authorized. Exchanging code for tokens...")
	af, err := logincore.ExchangeCodeForTokens(opts, provider.CodexOAuthClientID, cs.AuthorizationCode, cs.CodeVerifier)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logincore.DirOf(authFile), 0o700); err != nil {
		return err
	}
	if err := provider.WriteCodexAuthFile(authFile, af); err != nil {
		return err
	}
	fmt.Printf("✓ codex OAuth tokens saved to %s\n", authFile)
	if af.Tokens.AccountID != "" {
		fmt.Printf("  account_id: %s\n", af.Tokens.AccountID)
	}
	fmt.Println("\nYou can now use codex-native models (gpt-5.5) through the proxy.")
	return nil
}
