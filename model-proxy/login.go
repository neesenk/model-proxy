package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Compass SSO login flow (ported from ais-switch-cli/internal/gateway/login.go + loopback.go):
//  1. GET auth/login → 401 + SSO_A cookie (in jar) + result login URL
//  2. Set next=<loopback callback> on the login URL, open browser
//  3. User completes Google login on soup.shopee.io; wait for EITHER the loopback
//     callback (browser hits us, local case) OR the user pressing ENTER on stdin
//     (remote/SSH case where the browser can't reach this machine's loopback port).
//     The signal only marks "login complete" — SSO_A stays in the CLI's jar the whole time.
//  4. On callback/ENTER signal, poll auth/info (jar carries SSO_A) until authed;
//     the 200 sets SSO_C, captured by the jar.
//  5. Persist account (SSO_C, project_id, email, ...)
//  6. Provision managed API key via get_or_generate (cookie-authed), filling in identity

func cmdLogin(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := positional(args)
	if provName == "" {
		fmt.Println("usage: model-proxy login <provider>")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (auth=%s)\n", name, p.Auth)
		}
		return
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	switch prov.Auth {
	case "cqp":
		if err := runLogin(cfg); err != nil {
			log.Fatalf("login failed: %v", err)
		}
	case "codex_oauth":
		cmdCodexLogin(args)
	default:
		log.Fatalf("login not supported for provider %q (auth=%s)", provName, prov.Auth)
	}
}

func runLogin(cfg *Config) error {
	storePath := cfg.Auth.SSOCookieFile
	if storePath == "" {
		return fmt.Errorf("auth.sso_cookie_file not set in config")
	}
	c := newCompassClient(storePath)

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
	finalURL := withNextCallback(loginURL, ls.CallbackURL())

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
	if err := openBrowser(finalURL); err != nil {
		fmt.Printf("(could not open the browser automatically: %v — please open the link above manually)\n", err)
	}

	// 3. Wait for the loopback callback OR the user pressing ENTER. The former is
	//    the local case; the latter covers remote SSH. Either signal means "login
	//    complete"; the session (SSO_C) is then resolved by polling auth/info with
	//    the jar's SSO_A — no cookie needs to travel back from the browser.
	if err := waitForLoginSignal(ls, 5*time.Minute); err != nil {
		return err
	}
	fmt.Println("[GoogleGateway] Received login-complete signal, checking Compass session")
	fmt.Println("[GoogleGateway] Ignoring OAuth callback code/state (Compass Soup SSO flow)")

	// 4. Poll auth/info until retcode==0 && hasAccess. The jar (with SSO_A) is
	//    upgraded to SSO_C by the 200's Set-Cookie.
	if _, err := c.PollSession(3 * time.Minute); err != nil {
		return err
	}
	ssoCookie := c.SessionCookie()

	// 5. Provision the managed API key — get_or_generate returns the full identity
	//    (api_key + project_id + employee_*), so we call it first to populate the
	//    account, then persist.
	keyData, err := c.fetchAPIKey()
	if err != nil {
		return fmt.Errorf("api key provisioning: %w", err)
	}

	// 6. Persist the account.
	a := &AccountData{
		AccountID:        keyData.EmployeeEmail,
		Email:            keyData.EmployeeEmail,
		ProjectID:        keyData.ProjectID,
		SSOSessionCookie: ssoCookie,
		LastRefreshAt:    time.Now().Unix(),
	}
	if err := saveAccount(storePath, a); err != nil {
		return fmt.Errorf("failed to persist account: %w", err)
	}

	fmt.Printf("%s login complete: %s (project=%s)\n", cGreen("[GoogleGateway]"), cBold(cCyan(a.Email)), cGray(a.ProjectID))
	fmt.Printf("  %s %s\n", cDim("store:"), cGray(storePath))
	fmt.Printf("\n%s You can now run `%s` or `%s`.\n", cGreen("Login complete."), cCyan("model-proxy mint-key"), cCyan("model-proxy serve"))
	return nil
}

// withNextCallback sets the `next` query param on the soup.shopee.io login URL
// to the loopback callback so the browser returns after Google login.
func withNextCallback(loginURL, callback string) string {
	u, err := url.Parse(loginURL)
	if err != nil {
		return loginURL
	}
	q := u.Query()
	q.Set("next", callback)
	u.RawQuery = q.Encode()
	return u.String()
}

// waitForLoginSignal blocks until either the loopback server is hit by the
// browser (local login) or the user presses ENTER on stdin (remote/SSH login,
// where the browser cannot reach this machine's loopback port). Either signal
// means the user has completed the browser login and the CLI may proceed to
// poll auth/info with the SSO_A already in its cookie jar.
func waitForLoginSignal(ls *LoopbackServer, timeout time.Duration) error {
	done := make(chan struct{}, 1)
	// Loopback hit watcher.
	go func() {
		_, _ = ls.WaitForCookie(timeout)
		select {
		case done <- struct{}{}:
		default:
		}
	}()
	// Stdin ENTER watcher.
	go func() {
		br := bufio.NewReader(os.Stdin)
		_, _ = br.ReadString('\n')
		select {
		case done <- struct{}{}:
		default:
		}
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("login timed out waiting for callback or ENTER")
	}
}

// ---- LoopbackServer (ported from ais-switch-cli/internal/gateway/loopback.go) ----

// LoopbackServer runs the local /company-gateway/login-complete callback server.
// It captures the SSO_A cookie from the browser redirect.
type LoopbackServer struct {
	addr     string
	cookieCh chan string
	errCh    chan error
	srv      *http.Server
	port     int
}

// NewLoopbackServer binds an ephemeral loopback port.
func NewLoopbackServer() (*LoopbackServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return &LoopbackServer{
		addr:     fmt.Sprintf("127.0.0.1:%d", port),
		port:     port,
		cookieCh: make(chan string, 1),
		errCh:    make(chan error, 1),
	}, nil
}

// Port returns the bound port.
func (l *LoopbackServer) Port() int { return l.port }

// CallbackURL returns the full callback URL.
func (l *LoopbackServer) CallbackURL() string {
	return fmt.Sprintf("http://%s%s", l.addr, loginCompletePath)
}

// Start begins serving (non-blocking). The listener is bound synchronously so
// the callback URL is reachable as soon as Start returns.
func (l *LoopbackServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc(loginCompletePath, l.handle)
	l.srv = &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		return fmt.Errorf("failed to start local login success page: %w", err)
	}
	l.addr = ln.Addr().String() // resolve actual addr
	go func() {
		if err := l.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			l.errCh <- err
		}
	}()
	return nil
}

func (l *LoopbackServer) handle(w http.ResponseWriter, r *http.Request) {
	// Validate callback origin (scheme http(s), must have host, no path/query/fragment beyond the registered path).
	if err := validateOrigin(r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		l.errCh <- err
		return
	}
	// The loopback callback is the terminal step of the SSO redirect chain.
	// After Google login: soup.shopee.io -> compass (sets SSO_C) -> here, the
	// browser carries SSO_A/SSO_C cookies on this request. Capture whichever is
	// present (preferring SSO_C). Ignore code/state (Compass Soup SSO flow).
	var ssoC, ssoA string
	for _, c := range r.Cookies() {
		switch c.Name {
		case ssoCookieName: // SSO_C
			ssoC = c.Value
		case "SSO_A":
			ssoA = c.Value
		}
	}
	if ssoC == "" {
		ssoC = r.URL.Query().Get(ssoCookieName)
	}
	fmt.Printf("[GoogleGateway] Received SSO callback signal; checking Compass session (SSO_C=%v SSO_A=%v)\n",
		ssoC != "", ssoA != "")

	// Respond with the success page.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, successHTML)

	// Signal completion. Prefer SSO_C; fall back to SSO_A; else a bare signal
	// (the session is later resolved by polling auth/info with the jar's SSO_A).
	cookie := ssoC
	if cookie == "" {
		cookie = ssoA
	}
	if cookie == "" {
		cookie = "callback-signal"
	}
	select {
	case l.cookieCh <- cookie:
	default:
	}
}

// WaitForCookie blocks until the cookie arrives or times out.
func (l *LoopbackServer) WaitForCookie(timeout time.Duration) (string, error) {
	select {
	case c := <-l.cookieCh:
		return c, nil
	case err := <-l.errCh:
		return "", err
	case <-time.After(timeout):
		return "", fmt.Errorf("login success page timed out")
	}
}

// Stop shuts down the server.
func (l *LoopbackServer) Stop() {
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.srv.Shutdown(ctx)
	}
}

func validateOrigin(r *http.Request) error {
	origin := r.Host
	if origin == "" {
		return fmt.Errorf("callback origin missing host")
	}
	// Allow only loopback hosts.
	host, _, err := net.SplitHostPort(origin)
	if err != nil {
		host = origin
	}
	if host != "127.0.0.1" && host != "localhost" {
		return fmt.Errorf("unsupported callback origin host: %s", host)
	}
	if r.URL.Path != loginCompletePath {
		return fmt.Errorf("callback origin must not include path beyond %s", loginCompletePath)
	}
	return nil
}

const successHTML = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>model-proxy</title>
<style>body{font-family:system-ui,sans-serif;text-align:center;padding:3rem}h1{color:#16a34a}</style>
</head><body><h1>✓ Login successful</h1>
<p>You can close this window and return to the terminal.</p>
<p style="color:#666;font-size:.9em">If you're logging in over SSH, this page may not open — that's normal; just return to the terminal and press ENTER.</p>
</body></html>`

// openBrowser opens a URL in the default browser (cross-platform).
func openBrowser(rawurl string) error {
	switch runtimeOS() {
	case "darwin":
		return runCmd("open", rawurl)
	case "windows":
		return runCmd("rundll32", "url.dll,FileProtocolHandler", rawurl)
	default: // linux/bsd
		for _, b := range []string{"xdg-open", "x-www-browser", "www-browser"} {
			if err := runCmd(b, rawurl); err == nil {
				return nil
			}
		}
		return fmt.Errorf("no browser launcher found")
	}
}
