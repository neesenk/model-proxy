package login

import (
	"bufio"
	"fmt"
	"log"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	"os"
	"path/filepath"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/provider"
)

// runApiKeyLogin is a no-label/no-replace convenience wrapper over
// runApiKeyLoginWithInput (prompts on stdin, writes the plural credential pool
// <name>_apikeys.json so repeated logins accumulate accounts). Kept for the
// login_cmd tests; the `login` CLI calls runApiKeyLoginWithInput directly.
func RunApiKeyLogin(cfg *configdomain.Config, provName string, prov configdomain.Provider) error {
	return RunApiKeyLoginWithInput(cfg, provName, prov, "", "", false)
}

// runApiKeyLoginWithInput performs a pool-aware apikey login. The key may be
// passed directly (tests, or a future --key flag) or, when empty, prompted on
// stdin. The key is validated against the provider's usage_url if configured
// (401/403 rejects). The account is deduped by id (accountIDFor): a new id
// appends; an existing id with replace=true (or an interactive `y` on stdin
// when replace=false) overwrites the entry's key/label in place; an existing
// id without confirmation aborts with "login cancelled". The entry's label
// defaults to the id when not supplied. The pool is written to
// ~/.model-proxy/<name>_apikeys.json via savePool.
//
// Locking: ALL stdin (key prompt + replace confirmation) and the usage-URL
// validation happen BEFORE the cross-process lock — a holder who walks away
// mid-prompt would otherwise stall every other login/logout for the 60s stale
// window. The replace confirmation is resolved with a read-only loadPool before
// the lock; the authoritative load→dedup→save then runs under withPoolLock (in
// addApikeyAccount). The read-twice is safe: the inside-lock load re-finds the
// entry by id (which may have changed between the two loads), so a concurrent
// mutation is reconciled rather than clobbered.
//
// This is the CLI wrapper: it owns stdin prompting + stdout printing, then
// delegates the validate→dedup→save core to addApikeyAccount (reused by the
// web layer, Task 12).
func RunApiKeyLoginWithInput(cfg *configdomain.Config, provName string, prov configdomain.Provider, in, label string, replace bool) error {
	// === BEFORE LOCK: key prompt ===
	key := strings.TrimSpace(in)
	if key == "" {
		fmt.Printf("Enter API key for %s: ", provName)
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("read API key: %w", err)
		}
		key = strings.TrimSpace(line)
	}
	if key == "" {
		return fmt.Errorf("empty API key")
	}
	if ApiKeyValidationURL(prov) != "" {
		fmt.Fprintf(os.Stderr, "Validating API key...\n")
	}

	// Resolve the replace confirmation BEFORE the lock (stdin must never block
	// the cross-process lock). A read-only loadPool + scan for the id decides
	// whether to prompt; if the user declines, abort without acquiring the lock.
	id := accountIDFor(prov.Provider, accountCred{APIKey: key})
	if !replace {
		existing, err := loadPool(provName, prov.Provider)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		for _, a := range existing.Accounts {
			if a.ID == id {
				fmt.Printf("Account %q is already logged in. Replace its key? [y/N] ", a.Label)
				reader := bufio.NewReader(os.Stdin)
				ans, _ := reader.ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
					return fmt.Errorf("login cancelled")
				}
				break
			}
		}
		replace = true // user confirmed; tell the core to overwrite
	}

	if _, err := AddApikeyAccount(cfg, provName, prov, accountCred{APIKey: key}, label, replace); err != nil {
		return err
	}
	// Print the confirmation line (label resolved from the freshly-saved pool,
	// which may have been re-sorted by savePool).
	pool, _ := loadPool(provName, prov.Provider)
	fmt.Println(provider.Green("✓ Saved account ") + provider.Gray(cliframework.Mask(id)+" ("+labelFor(pool, id)+")"))
	return nil
}

// apiKeyValidationURL returns the endpoint used to validate an API key at login:
// prov.UsageURL when set, otherwise openai_base_url + "/models" (the natural
// Bearer-GET probe), or "" when neither is set (login skips validation). It is
// field-based (not provider_id-based): providers with a real usage API set
// usage_url (zhipu/deepseek/volcengine/kimi-code → unchanged); providers without
// one (qwen-plan) validate against openai_base_url/models. Shared by the
// "Validating…" message gate and addApikeyAccount's validateKeyBearerGET call.
func ApiKeyValidationURL(prov configdomain.Provider) string {
	if prov.UsageURL != "" {
		return prov.UsageURL
	}
	if prov.OpenAIBaseURL != "" {
		return strings.TrimRight(prov.OpenAIBaseURL, "/") + "/models"
	}
	return ""
}

// addApikeyAccount is the non-printing core extracted from
// runApiKeyLoginWithInput: it validates the key against usage_url (if set),
// dedups by id under the cross-process lock, and writes the pool. Returns the
// account id. No stdin, no stdout — the CLI wrapper (or the web layer) handles
// UX. Callers decide replace semantics: the CLI resolves it via an interactive
// prompt BEFORE calling this; the web layer passes the client's choice.
//
// replace=false on an existing id returns "login cancelled" without modifying
// the pool — callers surface that error as appropriate (CLI prints, web 409s).
func AddApikeyAccount(cfg *configdomain.Config, name string, prov configdomain.Provider, cred accountCred, label string, replace bool) (string, error) {
	key := strings.TrimSpace(cred.APIKey)
	if key == "" {
		return "", fmt.Errorf("empty API key")
	}
	// Validate against the usage endpoint if configured. 401/403 = key invalid;
	// anything else (200, 404, etc.) = key accepted (the endpoint may not exist,
	// but the key itself was not rejected).
	if err := ValidateKeyBearerGET(ApiKeyValidationURL(prov), key); err != nil {
		return "", err
	}
	id := accountIDFor(prov.Provider, accountCred{APIKey: key})
	return id, withPoolLock(name, func() error {
		pool, err := loadPool(name, prov.Provider)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		now := nowTS()
		idx := -1
		for i, a := range pool.Accounts {
			if a.ID == id {
				idx = i
				break
			}
		}
		if idx >= 0 {
			if !replace {
				return fmt.Errorf("login cancelled")
			}
			pool.Accounts[idx].APIKey = key
			if label != "" {
				pool.Accounts[idx].Label = label
			}
			pool.Accounts[idx].AddedAt = now
		} else {
			lbl := label
			if lbl == "" {
				lbl = id
			}
			pool.Accounts = append(pool.Accounts, poolAccount{ID: id, Label: lbl, APIKey: key, AddedAt: now})
		}
		return savePool(name, prov.Provider, pool)
	})
}

// removeApikeyAccount removes the account with the given id from the named
// pool under the cross-process lock. No-op if the id is absent (no error). No
// stdin, no stdout — symmetric with addApikeyAccount, reused by the web layer.
func RemoveApikeyAccount(name, providerID, id string) error {
	return withPoolLock(name, func() error {
		pool, err := loadPool(name, providerID)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		out := pool.Accounts[:0]
		for _, a := range pool.Accounts {
			if a.ID != id {
				out = append(out, a)
			}
		}
		pool.Accounts = out
		return savePool(name, providerID, pool)
	})
}

func RunLogin(cfg *configdomain.Config, provName string) error {
	storePath := filepath.Join(HomeDir(), ".model-proxy", provName+"_oauth_auth.json")
	c := NewAqpClient(storePath)

	// 1. Bootstrap: get the login URL + SSO_A cookie.
	loginURL, err := c.BootstrapLoginURL()
	if err != nil {
		return err
	}

	// 2. Loopback callback server.
	ls, err := NewLoopbackServer()
	if err != nil {
		return fmt.Errorf("failed to allocate loopback port: %w", err)
	}
	if err := ls.Start(); err != nil {
		return fmt.Errorf("failed to start local login success page: %w", err)
	}
	defer ls.Stop()

	// Set the loopback callback as `next` on the soup.shopee.io login URL.
	finalURL := WithNextCallback(loginURL, ls.CallbackURL())

	fmt.Printf(`
Open a browser (or copy the link below into one) to complete your company Google login:
  %s

After logging in, the browser will try to redirect back to this machine:
  %s

  • If the page shows "✓ Login successful" — you're done, come back here.
  • If the connection fails (e.g. you're logging in over SSH, or the port is
    unreachable) — that's normal, just come back here and press ENTER.
`, finalURL, ls.CallbackURL())
	fmt.Println("Once you've finished logging in (or the redirect above succeeded), come back here and press ENTER...")
	// Try to open the browser automatically (works locally); on remote/SSH it
	// usually fails — the user can copy the link above manually.
	if err := OpenBrowser(finalURL); err != nil {
		fmt.Printf("(could not open the browser automatically: %v — please open the link above manually)\n", err)
	}

	// 3. Wait for the loopback callback OR the user pressing ENTER. The former is
	//    the local case; the latter covers remote SSH. Either signal means "login
	//    complete"; the session (SSO_C) is then resolved by polling auth/info with
	//    the jar's SSO_A — no cookie needs to travel back from the browser.
	if err := WaitForLoginSignal(ls, 5*time.Minute); err != nil {
		return err
	}
	fmt.Println("[GoogleGateway] Received login-complete signal, checking AQP session")
	fmt.Println("[GoogleGateway] Ignoring OAuth callback code/state (AQP Soup SSO flow)")

	// 4. Poll auth/info until retcode==0 && hasAccess. The jar (with SSO_A) is
	//    upgraded to SSO_C by the 200's Set-Cookie.
	if _, err := c.PollSession(3 * time.Minute); err != nil {
		return err
	}
	ssoCookie := c.SessionCookie()

	// 5. Provision the managed API key — get_or_generate returns the full identity
	//    (api_key + project_id + employee_*), so we call it first to populate the
	//    account, then persist.
	keyData, err := c.FetchAPIKey()
	if err != nil {
		return fmt.Errorf("api key provisioning: %w", err)
	}

	// 6. Persist the account.
	a := &provider.AqpAccountData{
		AccountID:        keyData.EmployeeEmail,
		Email:            keyData.EmployeeEmail,
		ProjectID:        keyData.ProjectID,
		SSOSessionCookie: ssoCookie,
		LastRefreshAt:    time.Now().Unix(),
	}
	if err := provider.SaveAqpAccount(storePath, a); err != nil {
		return fmt.Errorf("failed to persist account: %w", err)
	}

	fmt.Printf("%s login complete: %s (project=%s)\n", provider.Green("[GoogleGateway]"), provider.Bold(provider.Cyan(a.Email)), provider.Gray(a.ProjectID))
	fmt.Printf("  %s %s\n", provider.Dim("store:"), provider.Gray(storePath))
	fmt.Printf("\n%s You can now run `%s`.\n", provider.Green("Login complete."), provider.Cyan("model-proxy serve"))
	return nil
}

// CmdLogin is the process-level login entry: it loads config, dispatches the
// provider login flow, and signals a running daemon to hot-reload.
func CmdLogin(args []string) {
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := cliframework.Positional(args)
	if provName == "" {
		fmt.Println("usage: model-proxy login <provider> [--label <name>] [--replace]")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (provider=%s)\n", name, p.Provider)
		}
		return
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, cliframework.ProviderNames(cfg))
	}
	label := cliframework.FlagStringValue(args, "--label")
	replace := cliframework.HasFlagValue(args, "--replace")

	switch prov.Provider {
	case "aqp":
		if err := RunLogin(cfg, provName); err != nil {
			log.Fatalf("login failed: %v", err)
		}
	case "codex":
		CmdCodexLogin(provName)
	case "zcode":
		fmt.Println("Opening BigModel login to fetch a Coding Plan API key…")
		if err := OpenBrowser("https://bigmodel.cn/login"); err != nil {
			fmt.Fprintf(os.Stderr, "(could not open browser: %v — open https://bigmodel.cn/login manually)\n", err)
		}
		if err := RunApiKeyLoginWithInput(cfg, provName, prov, "", label, replace); err != nil {
			log.Fatalf("login failed: %v", err)
		}
	default:
		var err error
		if prov.Provider == "volcengine" {
			err = RunVolcengineLoginWithInput(cfg, provName, prov, "", "", "", label, replace)
		} else {
			err = RunApiKeyLoginWithInput(cfg, provName, prov, "", label, replace)
		}
		if err != nil {
			log.Fatalf("login failed: %v", err)
		}
	}
	// After ANY successful login, signal a running serve to hot-reload.
	cliserve.MaybeReloadDaemon(cfg)
}
