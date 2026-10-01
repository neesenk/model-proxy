package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/provider"
)

// diag_test.go — the daemon-twin diagnostics commands: replay (loopback
// re-send with one-shot force-provider), route test (per-target real probe),
// models.dev catalog pull.

// replayRig builds a Service with a request-log directory holding one record
// and a cfg whose Listen points at the given upstream-capture server.
func replayRig(t *testing.T, srv *httptest.Server) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	line := `{"ts":"2026-07-29T12:00:00Z","request_id":"r1","called_model":"m","provider":"p","status":200,` +
		`"path":"/v1/messages","request_body":"{\"model\":\"m\",\"messages\":[]}","response_body":"{}"}`
	if err := os.WriteFile(filepath.Join(dir, "requests-20260729.log"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: strings.TrimPrefix(srv.URL, "http://"),
		Providers: map[string]configdomain.Provider{
			"deepseek": {Provider: "deepseek"},
		},
	}
	service := New(Ports{
		Config:              func() *configdomain.Config { return cfg },
		RequestLogDirectory: func() string { return dir },
	})
	return service, dir
}

func TestReplayRoundTrip(t *testing.T) {
	var gotProvider, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProvider = r.Header.Get("x-mp-force-provider")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	service, _ := replayRig(t, srv)

	res, err := service.Replay(context.Background(), "r1", "deepseek")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res.Status != 200 || !strings.Contains(res.Body, `"ok":true`) {
		t.Errorf("result = %+v", res)
	}
	if gotProvider != "deepseek" {
		t.Errorf("force-provider header = %q, want deepseek", gotProvider)
	}
	if !strings.Contains(gotBody, `"model":"m"`) {
		t.Errorf("replayed body = %q", gotBody)
	}
	if res.LatencyMs < 0 {
		t.Errorf("latency = %d", res.LatencyMs)
	}
}

func TestReplayGuards(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	service, dir := replayRig(t, srv)

	// Unknown / empty arguments.
	for _, tc := range [][2]string{{"", "deepseek"}, {"r1", ""}, {"r1", "nope"}} {
		if _, err := service.Replay(context.Background(), tc[0], tc[1]); err == nil {
			t.Errorf("Replay(%q, %q) succeeded, want rejection", tc[0], tc[1])
		}
	}
	// Missing record → 404.
	_, err := service.Replay(context.Background(), "ghost", "deepseek")
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 404 {
		t.Errorf("missing record err = %v, want 404", err)
	}
	// Shadow records are never replayable (same guard as the CLI).
	shadow := `{"ts":"2026-07-29T12:00:01Z","request_id":"shadow-r1","called_model":"m","provider":"p","status":200,"path":"/v1/messages","request_body":"{}"}`
	f, _ := os.OpenFile(filepath.Join(dir, "requests-20260729.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(shadow + "\n")
	f.Close()
	if _, err := service.Replay(context.Background(), "shadow-r1", "deepseek"); err == nil ||
		!strings.Contains(err.Error(), "shadow") {
		t.Errorf("shadow replay err = %v, want the shadow guard", err)
	}
	// Request log disabled → clear 400.
	noLog := New(Ports{Config: func() *configdomain.Config {
		return &configdomain.Config{Providers: map[string]configdomain.Provider{"deepseek": {Provider: "deepseek"}}}
	}})
	if _, err := noLog.Replay(context.Background(), "r1", "deepseek"); err == nil ||
		!strings.Contains(err.Error(), "request_log is disabled") {
		t.Errorf("disabled request log err = %v", err)
	}
}

// TestReplayRejectsTruncatedBody: the fourth CLI-parity guard — a record whose
// request body was truncated by request_log.max_body_bytes must be refused
// BEFORE any loopback call (replaying an incomplete body would send a corrupt
// request upstream). The record is produced by the real requestlog Logger with
// a small capture limit, so the truncation marker is the genuine article, not
// a hand-written suffix.
func TestReplayRejectsTruncatedBody(t *testing.T) {
	var upstreamHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	logger := requestlog.New(requestlog.Options{
		Directory:    dir,
		MaxFileSize:  1 << 20,
		MaxBodyBytes: 16, // any body longer than this is truncated at capture
		Retention:    time.Hour,
	})
	go logger.Run()
	rec := logger.BuildRecord(requestlog.Input{
		RequestID:    "tr1",
		Path:         "/v1/messages",
		CalledModel:  "m",
		Provider:     "p",
		Status:       200,
		RequestBody:  []byte(`{"model":"m","messages":[{"role":"user","content":"a request body far beyond sixteen bytes"}]}`),
		ResponseBody: []byte(`{}`),
	})
	logger.Enqueue(rec)
	logger.Shutdown() // drains and waits: the record is on disk before Replay

	cfg := &configdomain.Config{
		Listen: strings.TrimPrefix(srv.URL, "http://"),
		Providers: map[string]configdomain.Provider{
			"deepseek": {Provider: "deepseek"},
		},
	}
	service := New(Ports{
		Config:              func() *configdomain.Config { return cfg },
		RequestLogDirectory: func() string { return dir },
	})

	_, err := service.Replay(context.Background(), "tr1", "deepseek")
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		t.Fatalf("truncated replay err = %v, want 400 HTTPError", err)
	}
	if !strings.Contains(err.Error(), "max_body_bytes") {
		t.Errorf("truncated replay err = %v, want the max_body_bytes guidance", err)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 0 {
		t.Fatalf("upstream hits = %d, want 0 — the guard must fire before the loopback call", got)
	}
}

func TestReplayNilConfig(t *testing.T) {
	dir := t.TempDir()
	line := `{"ts":"2026-07-29T12:00:00Z","request_id":"r1","path":"/v1/messages","request_body":"{}"}`
	if err := os.WriteFile(filepath.Join(dir, "requests-20260729.log"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Ports{
		Config:              func() *configdomain.Config { return nil },
		RequestLogDirectory: func() string { return dir },
	})
	_, err := service.Replay(context.Background(), "r1", "deepseek")
	if httpErrorStatus(t, err) != http.StatusServiceUnavailable {
		t.Fatalf("nil config err = %v, want 503", err)
	}
}

func TestTestRoute(t *testing.T) {
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"p1": {Provider: "deepseek", Models: []string{"m1"}, Priority: 2},
			"p2": {Provider: "deepseek", Models: []string{"m1"}, Priority: 1},
		},
	}
	service := New(Ports{
		Config: func() *configdomain.Config { return cfg },
		ProbeRuntime: func() (*configdomain.Config, map[string]provider.Provider) {
			return cfg, map[string]provider.Provider{}
		},
		// No impls built (not logged in) — the per-target failure is data,
		// never an abort.
		RouteProbeImpl: func(name string) provider.Provider { return nil },
	})
	result, err := service.TestRoute(context.Background(), "m1")
	if err != nil {
		t.Fatalf("TestRoute: %v", err)
	}
	if result.Model != "m1" || len(result.Results) != 2 {
		t.Fatalf("result = %+v", result)
	}
	// Priority order: p2 (priority 1) before p1 (priority 2).
	if result.Results[0].Provider != "p2" || result.Results[1].Provider != "p1" {
		t.Errorf("order = %s, %s — want priority order p2, p1", result.Results[0].Provider, result.Results[1].Provider)
	}
	for _, r := range result.Results {
		if r.OK || !strings.Contains(r.Reason, "not available") {
			t.Errorf("target = %+v, want ok:false with the not-available reason", r)
		}
	}

	// Unknown model → 404 with the available routes listed.
	_, err = service.TestRoute(context.Background(), "ghost")
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 404 || !strings.Contains(err.Error(), "m1") {
		t.Errorf("unknown model err = %v, want 404 listing routes", err)
	}
	// Empty model → 400.
	if _, err := service.TestRoute(context.Background(), ""); err == nil {
		t.Error("empty model accepted")
	}
}

// TestTestRouteProbesLiveTargets drives the impl-non-nil branch of TestRoute:
// a target with a real provider impl is probed against the upstream via
// probe.Exchange (priority order preserved) and its data row carries
// ok/status/latency; a route target whose provider is missing from the config
// surfaces the "provider not in config" data row instead of aborting.
func TestTestRouteProbesLiveTargets(t *testing.T) {
	var probeHits int32
	var gotPath, gotAuth, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probeHits, 1)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"live": {Provider: "deepseek", OpenAIBaseURL: up.URL, Models: []string{"m-live"}, Priority: 1},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m-live": {
				{Provider: "live", Model: "m-live", Priority: 1},
				{Provider: "ghost", Model: "m-live", Priority: 2}, // provider absent from config: defensive data row
			},
		},
	}
	service := New(Ports{
		Config: func() *configdomain.Config { return cfg },
		ProbeRuntime: func() (*configdomain.Config, map[string]provider.Provider) {
			return cfg, map[string]provider.Provider{}
		},
		RouteProbeImpl: func(name string) provider.Provider {
			if name == "live" {
				return &refreshFakeProv{}
			}
			return nil
		},
	})
	result, err := service.TestRoute(context.Background(), "m-live")
	if err != nil {
		t.Fatalf("TestRoute: %v", err)
	}
	if result.Model != "m-live" || len(result.Results) != 2 {
		t.Fatalf("result = %+v, want two data rows", result)
	}
	// Row 0: the live target probed the upstream through the real exchange.
	live := result.Results[0]
	if !live.OK || live.Provider != "live" || live.HTTPStatus != 200 || live.Reason != "" || live.LatencyMs < 0 {
		t.Errorf("live row = %+v, want ok:true status 200 with no reason", live)
	}
	// The probe used the impl's request shape and credentials.
	if gotPath != "/chat/completions" || gotAuth != "Bearer k" || !strings.Contains(gotBody, `"model":"m-live"`) {
		t.Errorf("probe upstream exchange = path %q auth %q body %q", gotPath, gotAuth, gotBody)
	}
	// Row 1: the missing provider is per-target data, never an abort.
	ghost := result.Results[1]
	if ghost.OK || ghost.Provider != "ghost" || ghost.Reason != "provider not in config" {
		t.Errorf("ghost row = %+v, want ok:false with the not-in-config reason", ghost)
	}
	// A 200 verdict is final: the cross-leg fallback must not re-probe.
	if got := atomic.LoadInt32(&probeHits); got != 1 {
		t.Fatalf("upstream probe hits = %d, want exactly one", got)
	}
}

func TestPullModelsCatalog(t *testing.T) {
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"zhipuai":{"models":{"glm-4.7":{"limit":{"context":128000,"output":8192}}}}}`))
	}))
	defer md.Close()
	t.Setenv("MP_MODELSDEV_URL", md.URL)
	home := t.TempDir()
	service := New(Ports{HomeDir: func() string { return home }})
	res, err := service.PullModelsCatalog(context.Background())
	if err != nil {
		t.Fatalf("PullModelsCatalog: %v", err)
	}
	if res.Status != "refreshed" || res.Count == 0 {
		t.Errorf("result = %+v", res)
	}
	// The cache file landed under the injected home — never the real HOME.
	if _, err := os.Stat(filepath.Join(home, ".model-proxy", "models_cache.json")); err != nil {
		t.Errorf("catalog cache not written under fake home: %v", err)
	}
}
