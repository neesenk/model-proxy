package web

import (
	"context"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// Options supplies the application ports and immutable presentation inputs for
// an admin server. It intentionally contains no composition-root type.
type Options struct {
	Reads     ReadAPI
	Commands  CommandAPI
	Version   string
	Assets    fs.FS
	AssetRoot string
	LogFile   func() string
}

// Server serves the admin UI and its JSON API.
type Server struct {
	reads     ReadAPI
	commands  CommandAPI
	version   string
	assets    fs.FS
	assetRoot string
	logFile   func() string
	tasks     *taskOwner
	sessions  *sessionStore
}

func New(opts Options) (*Server, error) {
	if err := requirePorts(opts.Reads, opts.Commands); err != nil {
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
	return &Server{reads: opts.Reads, commands: opts.Commands, version: opts.Version, assets: assets, assetRoot: root, logFile: opts.LogFile, tasks: newTaskOwner(), sessions: newSessionStore()}, nil
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/ui/", http.HandlerFunc(s.serveUI))
	mux.Handle("/api/", http.HandlerFunc(s.serveAPI))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
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

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
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
	w.Header().Set("content-type", contentTypeFor(name))
	_, _ = w.Write(data)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/api/status" && r.Method == http.MethodGet:
		s.handleStatus(w, r)
	case p == "/api/logs" && r.Method == http.MethodGet:
		s.handleLogs(w, r)
	case p == "/api/requests" && r.Method == http.MethodGet:
		s.handleRequestsList(w, r)
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
	case strings.HasPrefix(p, "/api/login/") && r.Method == http.MethodPost:
		s.handleLoginStart(w, r)
	case strings.HasPrefix(p, "/api/login/") && r.Method == http.MethodGet:
		s.handleLoginPoll(w, r)
	default:
		writeJSONErr(w, http.StatusNotFound, "no api route for "+p)
	}
}
