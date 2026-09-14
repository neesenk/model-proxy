package login

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	logincore "model-proxy/internal/login"
	"model-proxy/internal/provider"
)

// Credential import ("zero pasted tokens"): reuse an existing official CLI
// login (codex) or read keys from environment variables instead of typing
// secrets into the terminal. Imported values are NEVER echoed, logged, or
// included in error messages — errors name the file/field/variable only.

// codexCLIAuthPath is the official codex CLI credential file
// (~/.codex/auth.json), written by `codex login`.
func codexCLIAuthPath(homeDir string) string {
	return filepath.Join(homeDir, ".codex", "auth.json")
}

// codexCLIAuthFile mirrors the on-disk shape of the official codex CLI's
// auth.json (codex-rs): OAuth logins carry tokens.{id_token, access_token,
// refresh_token, account_id} + last_refresh; apikey-mode logins carry
// OPENAI_API_KEY instead (and no usable OAuth tokens).
type codexCLIAuthFile struct {
	OpenAIAPIKey string `json:"OPENAI_API_KEY"`
	Tokens       struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
}

// parseCodexCLIAuth parses the official codex CLI auth.json into the proxy's
// CodexAuthFile. The three OAuth tokens must all be non-empty; an apikey-mode
// codex CLI login (OPENAI_API_KEY set, no tokens) is rejected with a hint to
// use the interactive device flow instead. Error messages never quote file
// content (the file holds live tokens).
func parseCodexCLIAuth(path string, data []byte) (*provider.CodexAuthFile, error) {
	var src codexCLIAuthFile
	if err := json.Unmarshal(data, &src); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (re-run `codex login`, or use interactive `model-proxy login codex`)", path)
	}
	missing := []string{}
	for _, f := range []struct {
		name, value string
	}{
		{"access_token", src.Tokens.AccessToken},
		{"refresh_token", src.Tokens.RefreshToken},
		{"id_token", src.Tokens.IDToken},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, "tokens."+f.name)
		}
	}
	if len(missing) > 0 {
		if src.OpenAIAPIKey != "" {
			return nil, fmt.Errorf("%s is an apikey-mode codex CLI login (no OAuth tokens to import); use interactive `model-proxy login codex` instead", path)
		}
		return nil, fmt.Errorf("%s is missing %s (re-run `codex login`, or use interactive `model-proxy login codex`)", path, strings.Join(missing, ", "))
	}
	af := &provider.CodexAuthFile{AuthMode: "chatgpt"}
	af.Tokens.AccessToken = src.Tokens.AccessToken
	af.Tokens.RefreshToken = src.Tokens.RefreshToken
	af.Tokens.IDToken = src.Tokens.IDToken
	af.Tokens.AccountID = provider.AccountIDFromTokens(src.Tokens.IDToken, src.Tokens.AccountID)
	af.LastRefresh = src.LastRefresh
	if af.LastRefresh == "" {
		af.LastRefresh = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return af, nil
}

// RunCodexImport implements `login codex --from-codex`: it reads the official
// codex CLI login from ~/.codex/auth.json and saves it through the same store
// path as the interactive device flow (<provName>_oauth_auth.json), so the
// forward path / web UI / logout all see the imported credentials. The
// confirmation only prints a masked account id — never token material.
func RunCodexImport(provName string) error {
	srcPath := codexCLIAuthPath(HomeDir())
	data, err := os.ReadFile(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("codex CLI credentials not found at %s (run `codex login` first, or use interactive `model-proxy login codex`)", srcPath)
		}
		return fmt.Errorf("read %s: %w", srcPath, err)
	}
	af, err := parseCodexCLIAuth(srcPath, data)
	if err != nil {
		return err
	}
	authFile := oauthAuthFilePath(HomeDir(), provName)
	if err := os.MkdirAll(logincore.DirOf(authFile), 0o700); err != nil {
		return err
	}
	if err := provider.WriteCodexAuthFile(authFile, af); err != nil {
		return err
	}
	fmt.Printf("✓ Imported codex CLI credentials → %s\n", authFile)
	if af.Tokens.AccountID != "" {
		fmt.Printf("  account_id: %s\n", accounts.Mask(af.Tokens.AccountID))
	}
	fmt.Println("Note: the imported access_token may already be expired — the proxy refreshes it on demand while the refresh_token is valid.")
	return nil
}

// envCredentials reads login secrets from the named environment variables.
// keyVar is required; akVar/skVar are optional (volcengine). A missing or
// empty variable is an error naming the VARIABLE — never its value.
func envCredentials(keyVar, akVar, skVar string) (key, ak, sk string, err error) {
	read := func(name string) (string, error) {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return "", fmt.Errorf("environment variable %s is not set or empty", name)
		}
		return v, nil
	}
	if key, err = read(keyVar); err != nil {
		return "", "", "", err
	}
	if akVar != "" {
		if ak, err = read(akVar); err != nil {
			return "", "", "", err
		}
	}
	if skVar != "" {
		if sk, err = read(skVar); err != nil {
			return "", "", "", err
		}
	}
	return key, ak, sk, nil
}

// runFromEnvLogin implements `login <provider> --from-env VAR`: the key (and
// for volcengine the optional AK/SK pair) is read from environment variables,
// then saved through the exact same pool-aware path as interactive login —
// including --label/--replace semantics and the replace confirmation prompt.
func runFromEnvLogin(cfg *configdomain.Config, provName string, prov configdomain.Provider, keyVar, akVar, skVar, label string, replace bool) error {
	switch prov.Provider {
	case "codex":
		return fmt.Errorf("--from-env is not supported for codex (use --from-codex to import the codex CLI login, or interactive `model-proxy login codex`)")
	case "aqp":
		return fmt.Errorf("--from-env is not supported for aqp (SSO login only)")
	}
	if keyVar == "" {
		return fmt.Errorf("--from-env is required when using --from-env-ak/--from-env-sk")
	}
	if prov.Provider != "volcengine" && (akVar != "" || skVar != "") {
		return fmt.Errorf("--from-env-ak/--from-env-sk are only valid for volcengine")
	}
	key, ak, sk, err := envCredentials(keyVar, akVar, skVar)
	if err != nil {
		return err
	}
	if prov.Provider == "volcengine" {
		return runVolcengineLoginFromEnv(cfg, provName, prov, key, ak, sk, label, replace)
	}
	return RunApiKeyLoginWithInput(cfg, provName, prov, key, label, replace)
}

// runVolcengineLoginFromEnv is the --from-env variant of
// RunVolcengineLoginWithInput: all three values come from the environment, so
// it never prompts for the key/AK/SK themselves (AK/SK stay optional). The
// replace confirmation (the only remaining stdin interaction), validation,
// dedup, save and confirmation print are identical to the interactive flow.
func runVolcengineLoginFromEnv(cfg *configdomain.Config, provName string, prov configdomain.Provider, apiKey, ak, sk, label string, replace bool) error {
	id := accounts.AccountID(prov.Provider, accounts.Credentials{APIKey: apiKey, AccessKey: ak})
	if err := confirmReplace(provName, prov.Provider, id, replace); err != nil {
		return err
	}
	replace = true // user confirmed (or no duplicate); tell the core to overwrite
	if prov.UsageURL != "" || (ak != "" && sk != "") {
		fmt.Fprintf(os.Stderr, "Validating credentials...\n")
	}
	if _, err := logincore.AddVolcengineAccount(cfg, provName, prov, accounts.Credentials{APIKey: apiKey, AccessKey: ak, SecretKey: sk}, label, replace); err != nil {
		return err
	}
	printSaved(provName, prov.Provider, id)
	return nil
}
