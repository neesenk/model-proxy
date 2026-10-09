package daemonctl

// auth.go — the CLI-side half of the S2 auth layer: when the daemon listens
// on a non-loopback address both serving surfaces demand credentials, and the
// CLI's own daemon calls (status/doctor --live/replay/models/...) must
// present them. Credentials are wired ONCE per process from the loaded
// config (cliframework.LoadCmdConfig → SetAuthFiles) and attached
// automatically by request path: the admin token for /api, /metrics and
// /debug paths, one forward key for /v1 and /mcp paths. Unset (loopback
// deployments) keeps the historical no-header behavior; a configured file
// that cannot be read attaches nothing and the daemon answers the honest
// 401. Explicit caller headers are never overridden.

import (
	"io"
	"net/http"
	"strings"
	"sync"

	"model-proxy/internal/webauth"
)

var (
	authMu           sync.Mutex
	adminTokenPath   string
	apiKeyPath       string
	adminTokenCached string
	apiKeyCached     string
)

// SetAuthFiles registers the token files the daemon's two surfaces accept.
// Empty paths disable automatic auth (loopback deployments). Token values are
// read lazily on first use and cached for the process lifetime — CLI
// processes are short-lived, so mid-run rotation is out of scope by design.
func SetAuthFiles(adminFile, keysFile string) {
	authMu.Lock()
	defer authMu.Unlock()
	adminTokenPath = adminFile
	apiKeyPath = keysFile
	adminTokenCached = ""
	apiKeyCached = ""
}

func adminToken() string {
	authMu.Lock()
	defer authMu.Unlock()
	if adminTokenCached == "" && adminTokenPath != "" {
		adminTokenCached = webauth.FirstToken(adminTokenPath)
	}
	return adminTokenCached
}

func forwardKey() string {
	authMu.Lock()
	defer authMu.Unlock()
	if apiKeyCached == "" && apiKeyPath != "" {
		apiKeyCached = webauth.FirstToken(apiKeyPath)
	}
	return apiKeyCached
}

// credentialForPath picks the surface credential one path belongs to:
// /api, /metrics and /debug answer to the admin token; /v1 and /mcp answer
// to a forward key; anything else (e.g. /health) attaches nothing.
func credentialForPath(path string) string {
	for _, prefix := range []string{"/api", "/metrics", "/debug"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return adminToken()
		}
	}
	for _, prefix := range []string{"/v1", "/mcp"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return forwardKey()
		}
	}
	return ""
}

// Authorize attaches the path's surface credential to a request unless the
// caller already presented one. Exported so manual-request call sites (replay
// builds its own *http.Request) get the same treatment as Do/Get.
func Authorize(req *http.Request) {
	if req == nil {
		return
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("x-api-key") != "" {
		return
	}
	if tok := credentialForPath(req.URL.Path); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

// NewRequest builds a request against base+path with the surface credential
// pre-attached (Content-Type and body stay the caller's business; a
// *bytes.Reader / *strings.Reader body still sets ContentLength —
// http.NewRequest switches on the dynamic type).
func NewRequest(method, base, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, base+path, body)
	if err != nil {
		return nil, err
	}
	Authorize(req)
	return req, nil
}
