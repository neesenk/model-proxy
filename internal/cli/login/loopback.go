package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"time"

	"model-proxy/internal/provider"
)

const loginSignalPollInterval = 50 * time.Millisecond

type loginEnterSource interface {
	Poll(time.Duration) (bool, error)
}

type stdinEnterSource struct{ file *os.File }

func WithNextCallback(loginURL, callback string) string {
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
func WaitForLoginSignal(ls *LoopbackServer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := waitForLoginSignal(ctx, ls, &stdinEnterSource{file: os.Stdin})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("login timed out waiting for callback or ENTER: %w", err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("login canceled while waiting for callback or ENTER: %w", err)
	}
	return err
}

// waitForLoginSignal is the cancellable core shared by callback and ENTER
// completion. The stdin source performs bounded readiness polling, so callback,
// timeout, and cancellation never strand an uninterruptible reader goroutine.
func waitForLoginSignal(ctx context.Context, ls *LoopbackServer, enter loginEnterSource) error {
	for {
		select {
		case <-ls.CookieCh:
			return nil
		case err := <-ls.ErrCh:
			return fmt.Errorf("login callback failed: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if enter == nil {
			select {
			case <-ls.CookieCh:
				return nil
			case err := <-ls.ErrCh:
				return fmt.Errorf("login callback failed: %w", err)
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		pollFor := loginSignalPollInterval
		if deadline, ok := ctx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			if remaining < pollFor {
				pollFor = remaining
			}
		}
		pressed, err := enter.Poll(pollFor)
		if errors.Is(err, io.EOF) {
			// A closed/non-interactive stdin cannot provide ENTER. Disable that
			// branch and continue waiting for callback or context completion.
			enter = nil
			continue
		}
		if err != nil {
			return fmt.Errorf("read login confirmation: %w", err)
		}
		if pressed {
			return nil
		}
	}
}

func readLoginEnterByte(file *os.File) (bool, error) {
	var b [1]byte
	n, err := file.Read(b[:])
	if n > 0 {
		return b[0] == '\n' || b[0] == '\r', nil
	}
	return false, err
}

// ---- LoopbackServer ----

// LoopbackServer runs the local /company-gateway/login-complete callback server.
// It captures the SSO_A cookie from the browser redirect.
type LoopbackServer struct {
	addr     string
	CookieCh chan string
	ErrCh    chan error
	srv      *http.Server
	port     int
	ln       net.Listener
}

// NewLoopbackServer binds an ephemeral loopback port. The listener bound here
// is KEPT and handed to Start — re-listening the same address in Start would
// be a bind-close-rebind TOCTOU: another socket could take the port between
// the two calls, breaking the callback URL the login flow already printed.
func NewLoopbackServer() (*LoopbackServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().(*net.TCPAddr)
	return &LoopbackServer{
		addr:     fmt.Sprintf("127.0.0.1:%d", addr.Port),
		port:     addr.Port,
		ln:       ln,
		CookieCh: make(chan string, 1),
		ErrCh:    make(chan error, 1),
	}, nil
}

// Port returns the bound port.
func (l *LoopbackServer) Port() int { return l.port }

// CallbackURL returns the full callback URL.
func (l *LoopbackServer) CallbackURL() string {
	return fmt.Sprintf("http://%s%s", l.addr, LoginCompletePath)
}

// Start begins serving (non-blocking) on the listener bound at construction,
// so the callback URL is reachable as soon as Start returns — and on the very
// port that URL was derived from.
func (l *LoopbackServer) Start() error {
	if l.srv != nil {
		return fmt.Errorf("loopback server already started")
	}
	mux := http.NewServeMux()
	mux.HandleFunc(LoginCompletePath, l.handle)
	l.srv = &http.Server{Handler: mux}
	go func() {
		if err := l.srv.Serve(l.ln); err != nil && err != http.ErrServerClosed {
			l.ErrCh <- err
		}
	}()
	return nil
}

func (l *LoopbackServer) handle(w http.ResponseWriter, r *http.Request) {
	// Validate callback origin (scheme http(s), must have host, no path/query/fragment beyond the registered path).
	if err := ValidateOrigin(r); err != nil {
		// Reject the request and KEEP WAITING for the real callback: any local
		// process can hit this port with a crafted Host header, and writing the
		// error to ErrCh would abort the whole SSO login on that probe.
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	// The loopback callback is the terminal step of the SSO redirect chain.
	// After Google login: soup.shopee.io -> aqp (sets SSO_C) -> here, the
	// browser carries SSO_A/SSO_C cookies on this request. Capture whichever is
	// present (preferring SSO_C). Ignore code/state (AQP Soup SSO flow).
	var ssoC, ssoA string
	for _, c := range r.Cookies() {
		switch c.Name {
		case provider.SsoCookieName: // SSO_C
			ssoC = c.Value
		case "SSO_A":
			ssoA = c.Value
		}
	}
	if ssoC == "" {
		ssoC = r.URL.Query().Get(provider.SsoCookieName)
	}
	fmt.Printf("[GoogleGateway] Received SSO callback signal; checking AQP session (SSO_C=%v SSO_A=%v)\n",
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
	case l.CookieCh <- cookie:
	default:
	}
}

// WaitForCookie blocks until the cookie arrives or times out.
func (l *LoopbackServer) WaitForCookie(timeout time.Duration) (string, error) {
	select {
	case c := <-l.CookieCh:
		return c, nil
	case err := <-l.ErrCh:
		return "", err
	case <-time.After(timeout):
		return "", fmt.Errorf("login success page timed out")
	}
}

// Stop shuts down the server. Shutdown closes the served listener; when Start
// was never called the construction listener is closed directly so the port is
// always released.
func (l *LoopbackServer) Stop() {
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.srv.Shutdown(ctx)
		return
	}
	if l.ln != nil {
		_ = l.ln.Close()
	}
}

func ValidateOrigin(r *http.Request) error {
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
	if r.URL.Path != LoginCompletePath {
		return fmt.Errorf("callback origin must not include path beyond %s", LoginCompletePath)
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
func OpenBrowser(rawurl string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", rawurl).Run()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawurl).Run()
	default: // linux/bsd
		for _, b := range []string{"xdg-open", "x-www-browser", "www-browser"} {
			if err := exec.Command(b, rawurl).Run(); err == nil {
				return nil
			}
		}
		return fmt.Errorf("no browser launcher found")
	}
}
