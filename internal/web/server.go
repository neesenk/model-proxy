package web

import (
	"context"
	"encoding/base64"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"model-proxy/internal/appapi"
	"model-proxy/internal/webauth"
)

const (
	adminSessionCookie       = "mp_admin_session"
	adminSessionCookiePath   = "/api"
	maxAdminSessionCookieLen = 3800
)

// Options supplies the application ports and immutable presentation inputs for
// an admin server. It intentionally contains no composition-root type.
type Options struct {
	Reads     appapi.ReadAPI
	Commands  appapi.CommandAPI
	Version   string
	Assets    fs.FS
	AssetRoot string
	LogFile   func() string
	// BrowserListen is the configured daemon listen address. When admin auth is
	// enabled, its exact hostname is trusted in addition to IP-literal Hosts;
	// arbitrary DNS names remain blocked against rebinding.
	BrowserListen string
	// Events, when non-nil, serves the live SSE stream on GET /api/events.
	// The endpoint belongs to the /api/ subtree this transport registers, so
	// the composition root injects the hub-serving handler here — without it
	// the mux dispatches /api/events into this transport's 404 default while
	// the SSE branch in the proxy handler stays unreachable (web.enabled is
	// the default). Routing it through serveAPI also puts guardBrowserOrigin
	// in front of the stream.
	Events http.HandlerFunc
	// AdminAuth, when non-nil and enabled, gates /api/ and /metrics. /ui/ remains
	// a secret-free bootstrap document; it exchanges a valid bearer token for
	// an HttpOnly /api-scoped browser-session cookie. A closure (not a fixed
	// Source) lets reload-swapped config generations and token revocation take
	// effect without rebuilding the transport.
	AdminAuth func() *webauth.Source
}

// Server serves the admin UI and its JSON API.
type Server struct {
	reads         appapi.ReadAPI
	commands      appapi.CommandAPI
	events        http.HandlerFunc
	adminAuth     func() *webauth.Source
	version       string
	assets        fs.FS
	assetRoot     string
	logFile       func() string
	browserListen string
	tasks         *taskOwner
	sessions      *sessionStore
}

// adminAuthSnapshot captures the reload-swapped Source once for one transport
// request. Auth and LAN-origin policy must not observe different generations.
type adminAuthSnapshot struct {
	source  *webauth.Source
	enabled bool
}

func New(opts Options) (*Server, error) {
	if err := appapi.RequirePorts(opts.Reads, opts.Commands); err != nil {
		return nil, err
	}
	assets := opts.Assets
	if assets == nil {
		assets = defaultAssets()
	}
	root := strings.Trim(opts.AssetRoot, "/")
	if root == "" {
		root = "assets"
	}
	return &Server{reads: opts.Reads, commands: opts.Commands, events: opts.Events, adminAuth: opts.AdminAuth, version: opts.Version, assets: assets, assetRoot: root, logFile: opts.LogFile, browserListen: opts.BrowserListen, tasks: newTaskOwner(), sessions: newSessionStore()}, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/ui/", http.HandlerFunc(s.serveUI))
	mux.Handle("/api/", http.HandlerFunc(s.serveAPI))
	mux.Handle("/metrics", http.HandlerFunc(s.handleMetrics))
}

// Start begins transport-owned maintenance. It returns false after Close.
func (s *Server) Start() bool {
	return s.tasks.Run(func(ctx context.Context) {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sessions.GC()
			}
		}
	})
}

func (s *Server) Close() { s.tasks.Close() }

// GuardAdminBrowserOrigin applies the authenticated LAN variant to admin
// endpoints owned outside this transport (for example proxy-owned /debug/*).
// The caller must pass authEnabled and trustedHostPort from one captured config
// boundary; this function only evaluates browser identity headers.
func GuardAdminBrowserOrigin(w http.ResponseWriter, r *http.Request, authEnabled bool, trustedHostPort string) bool {
	return guardBrowserOrigin(w, r, authEnabled, trustedHostPort)
}

// guardBrowserOrigin enforces the loopback trust boundary for the browser-
// reachable admin surface (this transport's routes plus the proxy-owned
// endpoints reached via GuardAdminBrowserOrigin). Local CLI/curl clients send
// no Origin/Sec-Fetch-Site and pass untouched; requests that carry BROWSER
// identity headers must:
//
//   - carry a LOOPBACK Host (DNS rebinding serves attacker domains that resolve
//     here — same-origin from the browser's view, so only the Host check stops
//     it), and
//   - have an Origin that matches the request Host (a page on evil.com doing a
//     cross-site fetch — a CORS simple request with a text/plain body reaches
//     POST handlers — cannot forge this).
//
// GET /api/config answers with the verbatim YAML (static provider keys live in
// it), so reads need the same protection as mutations. allowAuthenticatedHost
// additionally accepts the configured listen host or a LAN IP for the
// authenticated variant.
func guardBrowserOrigin(w http.ResponseWriter, r *http.Request, allowAuthenticatedHost bool, trustedHostPort string) bool {
	origin := r.Header.Get("Origin")
	fetchSite := r.Header.Get("Sec-Fetch-Site")
	browser := origin != "" || fetchSite != ""
	if !browser {
		return true
	}
	if strings.EqualFold(fetchSite, "cross-site") {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return false
	}
	authenticatedHost := allowAuthenticatedHost && (isIPHostHeader(r.Host) || sameHostPort(r.Host, trustedHostPort))
	if !isLoopbackHostHeader(r.Host) && !authenticatedHost {
		http.Error(w, "admin API host must be loopback, an authenticated LAN IP, or the configured listen host", http.StatusForbidden)
		return false
	}
	if origin != "" {
		originHost := originHostPort(origin)
		if !strings.EqualFold(originHost, r.Host) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return false
		}
	}
	return true
}

func sameHostPort(got, configured string) bool {
	return configured != "" && strings.EqualFold(got, configured)
}

func isIPHostHeader(hostPort string) bool {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return net.ParseIP(host) != nil
}

func isLoopbackHostHeader(hostPort string) bool {
	host := hostPort
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originHostPort extracts "host[:port]" from an Origin header value.
func originHostPort(origin string) string {
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		return u.Host
	}
	return origin
}

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	s.serveEmbeddedUI(w, r, "/ui/", s.assetRoot)
}

// serveEmbeddedUI serves one static asset subtree. Embedded assets contain no
// runtime data or credential. Under admin auth they form the bootstrap page
// that collects a token and creates an HttpOnly API session; data endpoints
// remain fail-closed below.
func (s *Server) serveEmbeddedUI(w http.ResponseWriter, r *http.Request, prefix, root string) {
	auth := s.captureAdminAuth()
	if !guardBrowserOrigin(w, r, auth.enabled, s.browserListen) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, prefix)
	if name == "" || strings.HasSuffix(name, "/") {
		name = "index.html"
	}
	// Do not delegate this check solely to fs.ValidPath: test and alternate FS
	// implementations must receive the same explicit traversal rejection.
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			http.NotFound(w, r)
			return
		}
	}
	data, err := fs.ReadFile(s.assets, root+"/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Assets are embedded in the binary; a binary update must not be masked by a
	// browser holding a stale index.html/app.js. no-cache allows revalidation
	// while embedded FS reads stay cheap. nosniff is defense in depth.
	w.Header().Set("content-type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	auth := s.captureAdminAuth()
	p := r.URL.Path
	if p == "/api/auth/session" {
		if !guardBrowserOrigin(w, r, auth.enabled, s.browserListen) {
			return
		}
		switch r.Method {
		case http.MethodPost:
			s.handleAdminSessionCreate(w, r, auth)
		case http.MethodDelete:
			s.clearAdminSession(w, r)
		default:
			writeJSONErr(w, http.StatusMethodNotAllowed, "POST or DELETE only")
		}
		return
	}
	if !s.guardAdminAuth(w, r, auth) {
		return
	}
	if !guardBrowserOrigin(w, r, auth.enabled, s.browserListen) {
		return
	}
	switch {
	case p == "/api/events" && r.Method == http.MethodGet && s.events != nil:
		s.events(w, r)
	case p == "/api/status" && r.Method == http.MethodGet:
		s.handleStatus(w, r)
	case p == "/api/models" && r.Method == http.MethodGet:
		s.handleModels(w, r)
	case p == "/api/models/refresh" && r.Method == http.MethodPost:
		s.handleModelsRefresh(w, r)
	case p == "/api/logs" && r.Method == http.MethodGet:
		s.handleLogs(w, r)
	case p == "/api/requests" && r.Method == http.MethodGet:
		s.handleRequestsList(w, r)
	case p == "/api/mcp" && r.Method == http.MethodGet:
		s.handleMCPSurface(w, r)
	case p == "/api/mcp/test" && r.Method == http.MethodPost:
		s.handleMCPTest(w, r)
	case p == "/api/sessions" && r.Method == http.MethodGet:
		s.handleSessions(w, r)
	case p == "/api/security" && r.Method == http.MethodGet:
		s.handleSecurity(w, r)
	case p == "/api/security/explain" && r.Method == http.MethodGet:
		s.handleSecurityExplain(w, r)
	case p == "/api/security/blocks" && r.Method == http.MethodGet:
		s.handleSecurityBlocks(w, r)
	case p == "/api/security/adjudications" && r.Method == http.MethodGet:
		s.handleSecurityAdjudications(w, r)
	case strings.HasPrefix(p, "/api/security/blocks/") && r.Method == http.MethodDelete:
		s.handleSecurityUnblock(w, r)
	case strings.HasPrefix(p, "/api/requests/") && r.Method == http.MethodGet:
		s.handleRequestDetail(w, r)
	case p == "/api/shadow-report" && r.Method == http.MethodGet:
		s.handleShadowReport(w, r)
	case p == "/api/fusion" && r.Method == http.MethodGet:
		s.handleFusion(w, r)
	case p == "/api/config" && r.Method == http.MethodGet:
		s.handleConfigGet(w, r)
	case p == "/api/config" && r.Method == http.MethodPost:
		s.handleConfigPut(w, r)
	case p == "/api/config/validate" && r.Method == http.MethodPost:
		s.handleConfigValidate(w, r)
	case p == "/api/config/edit" && r.Method == http.MethodPost:
		s.handleConfigEdit(w, r)
	case p == "/api/accounts" && r.Method == http.MethodGet:
		s.handleAccountsList(w, r)
	case p == "/api/tokens" && r.Method == http.MethodGet:
		s.handleTokens(w, r)
	case p == "/api/tokens/reset" && r.Method == http.MethodPost:
		s.handleTokensReset(w, r)
	case p == "/api/quota/refresh" && r.Method == http.MethodPost:
		s.handleQuotaRefresh(w, r)
	case p == "/api/health/reset" && r.Method == http.MethodPost:
		s.handleHealthReset(w, r)
	case p == "/api/health/freeze" && r.Method == http.MethodPost:
		s.handleHealthFreeze(w, r)
	case p == "/api/stats" && r.Method == http.MethodGet:
		s.handleStats(w, r)
	case p == "/api/agents" && r.Method == http.MethodGet:
		s.handleAgents(w, r)
	case p == "/api/pin" && r.Method == http.MethodGet:
		s.handlePinList(w, r)
	case p == "/api/pin" && r.Method == http.MethodPost:
		s.handlePinSet(w, r)
	case p == "/api/pin" && r.Method == http.MethodDelete:
		s.handlePinClear(w, r)
	case p == "/api/analytics" && r.Method == http.MethodGet:
		s.handleAnalytics(w, r)
	case strings.HasPrefix(p, "/api/accounts/") && strings.HasSuffix(p, "/test") && r.Method == http.MethodPost:
		s.handleAccountTest(w, r)
	case strings.HasPrefix(p, "/api/accounts/") && r.Method == http.MethodPost:
		s.handleAccountAdd(w, r)
	case strings.HasPrefix(p, "/api/accounts/") && r.Method == http.MethodDelete:
		s.handleAccountRemove(w, r)
	case p == "/api/presets" && r.Method == http.MethodGet:
		s.handlePresetsList(w, r)
	case strings.HasPrefix(p, "/api/presets/") && r.Method == http.MethodPost:
		s.handlePresetAdd(w, r)
	case strings.HasPrefix(p, "/api/login/") && r.Method == http.MethodPost:
		s.handleLoginStart(w, r)
	case strings.HasPrefix(p, "/api/login/") && r.Method == http.MethodGet:
		s.handleLoginPoll(w, r)
	default:
		writeJSONErr(w, http.StatusNotFound, "no api route for "+p)
	}
}

// guardAdminAuth applies the S2 admin-surface bearer check to this
// transport's subtree. Disabled (no Source / not Enabled) keeps the
// historical loopback-trust behavior; validate rejects non-loopback listens
// without an admin token file, so the open mode stays reachable only on
// loopback. Same header conventions as the forward surface (Bearer or
// x-api-key) so curl stays symmetrical.
func (s *Server) guardAdminAuth(w http.ResponseWriter, r *http.Request, auth adminAuthSnapshot) bool {
	if !auth.enabled {
		return true
	}
	presented := webauth.BearerFromRequest(r.Header.Get)
	// An explicitly supplied header is authoritative: a bad header must not be
	// masked by a valid ambient browser cookie.
	headerPresented := r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != ""
	if !headerPresented && presented == "" && strings.HasPrefix(r.URL.Path, adminSessionCookiePath+"/") {
		presented = adminSessionToken(r)
	}
	if !auth.source.Accept(presented) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="model-proxy-admin"`)
		http.Error(w, "unauthorized: bad admin token", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) captureAdminAuth() adminAuthSnapshot {
	if s.adminAuth == nil {
		return adminAuthSnapshot{}
	}
	src := s.adminAuth()
	return adminAuthSnapshot{source: src, enabled: src != nil && src.Enabled()}
}

func adminSessionToken(r *http.Request) string {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || cookie.Value == "" || len(cookie.Value) > maxAdminSessionCookieLen {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return ""
	}
	return string(decoded)
}

func (s *Server) handleAdminSessionCreate(w http.ResponseWriter, r *http.Request, auth adminAuthSnapshot) {
	if !auth.enabled {
		writeJSONErr(w, http.StatusBadRequest, "admin auth is disabled")
		return
	}
	// Session bootstrap deliberately requires the Authorization bearer form;
	// x-api-key remains valid for direct API clients but cannot mint an ambient
	// browser credential through an accidentally inherited header convention.
	authorization := r.Header.Get("Authorization")
	token := webauth.BearerFromRequest(func(name string) string {
		if strings.EqualFold(name, "Authorization") {
			return authorization
		}
		return ""
	})
	if !auth.source.Accept(token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="model-proxy-admin"`)
		http.Error(w, "unauthorized: bad admin token", http.StatusUnauthorized)
		return
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(token))
	if len(encoded) > maxAdminSessionCookieLen {
		writeJSONErr(w, http.StatusBadRequest, "admin token is too long for a browser session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    encoded,
		Path:     adminSessionCookiePath,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearAdminSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    "",
		Path:     adminSessionCookiePath,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
