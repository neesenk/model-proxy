package web

import (
	"context"
	"io/fs"
	"model-proxy/internal/appapi"
	"model-proxy/internal/webauth"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	// Events, when non-nil, serves the live SSE stream on GET /api/events.
	// The endpoint belongs to the /api/ subtree this transport registers, so
	// the composition root injects the hub-serving handler here — without it
	// the mux dispatches /api/events into this transport's 404 default while
	// the SSE branch in the proxy handler stays unreachable (web.enabled is
	// the default). Routing it through serveAPI also puts guardBrowserOrigin
	// in front of the stream.
	Events http.HandlerFunc
	// AdminAuth, when non-nil and enabled, is the S2 admin-surface bearer
	// check applied to this transport's whole subtree (/api/, /ui/, /metrics).
	// A closure (not a fixed Source) so reload-swapped config generations are
	// picked up without rebuilding the transport.
	AdminAuth func() *webauth.Source
}

// Server serves the admin UI and its JSON API.
type Server struct {
	reads     appapi.ReadAPI
	commands  appapi.CommandAPI
	events    http.HandlerFunc
	adminAuth func() *webauth.Source
	version   string
	assets    fs.FS
	assetRoot string
	logFile   func() string
	tasks     *taskOwner
	sessions  *sessionStore
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
	return &Server{reads: opts.Reads, commands: opts.Commands, events: opts.Events, adminAuth: opts.AdminAuth, version: opts.Version, assets: assets, assetRoot: root, logFile: opts.LogFile, tasks: newTaskOwner(), sessions: newSessionStore()}, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/ui/", http.HandlerFunc(s.serveUI))
	mux.Handle("/api/", http.HandlerFunc(s.serveAPI))
	mux.Handle("/metrics", http.HandlerFunc(s.handleMetrics))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/metrics":
		s.handleMetrics(w, r)
	case strings.HasPrefix(r.URL.Path, "/ui/"):
		s.serveUI(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		s.serveAPI(w, r)
	default:
		http.NotFound(w, r)
	}
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

// GuardBrowserOrigin enforces the loopback trust boundary for the
// unauthenticated admin surface. Exported so the composition root can guard
// browser-reachable endpoints that ride the proxy handler instead of this
// transport (e.g. /debug/schedule, /api/events in web-disabled mode).
// admin surface. Local CLI/curl clients send no Origin/Sec-Fetch-Site and pass
// untouched; requests that carry BROWSER identity headers must:
//
//   - carry a LOOPBACK Host (DNS rebinding serves attacker domains that resolve
//     here — same-origin from the browser's view, so only the Host check stops
//     it), and
//   - have an Origin that matches the request Host (a page on evil.com doing a
//     cross-site fetch — a CORS simple request with a text/plain body reaches
//     POST handlers — cannot forge this).
//
// GET /api/config answers with the verbatim YAML (static provider keys live in
// it), so reads need the same protection as mutations.
func GuardBrowserOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	browser := origin != "" || r.Header.Get("Sec-Fetch-Site") != ""
	if !browser {
		return true
	}
	if !isLoopbackHostHeader(r.Host) {
		http.Error(w, "admin API host must be a loopback address", http.StatusForbidden)
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
	if !s.guardAdminAuth(w, r) {
		return
	}
	if !GuardBrowserOrigin(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
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
	data, err := fs.ReadFile(s.assets, s.assetRoot+"/"+name)
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
	if !s.guardAdminAuth(w, r) {
		return
	}
	if !GuardBrowserOrigin(w, r) {
		return
	}
	p := r.URL.Path
	switch {
	case p == "/api/events" && r.Method == http.MethodGet && s.events != nil:
		s.events(w, r)
	case p == "/api/status" && r.Method == http.MethodGet:
		s.handleStatus(w, r)
	case p == "/api/logs" && r.Method == http.MethodGet:
		s.handleLogs(w, r)
	case p == "/api/requests" && r.Method == http.MethodGet:
		s.handleRequestsList(w, r)
	case p == "/api/sessions" && r.Method == http.MethodGet:
		s.handleSessions(w, r)
	case p == "/api/security" && r.Method == http.MethodGet:
		s.handleSecurity(w, r)
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
func (s *Server) guardAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.adminAuth == nil {
		return true
	}
	src := s.adminAuth()
	if src == nil || !src.Enabled() {
		return true
	}
	if !src.Accept(webauth.BearerFromRequest(r.Header.Get)) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="model-proxy-admin"`)
		http.Error(w, "unauthorized: bad admin token", http.StatusUnauthorized)
		return false
	}
	return true
}
