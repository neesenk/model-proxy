package admin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/probe"
	"model-proxy/internal/routing"
)

// diag.go — the Web admin diagnostics commands: the daemon twins of
// `model-proxy replay <id> --to <provider>`, `model-proxy test <model>` and
// `model-proxy models pull`. CLI parity is deliberate: the replay guard rules
// (shadow record, non-/v1/ path, missing/truncated body) mirror
// internal/cli/diag/replay.go's DoReplay.

// replayBodyCap bounds the response body returned to the UI (the CLI streams
// unbounded to stdout; a browser page must not). SSE responses count too.
const replayBodyCap = 4 << 20

// Replay re-sends one logged request to the proxy's own forward endpoint with
// a one-shot x-mp-force-provider pin. The loopback HTTP call mirrors the CLI
// exactly (including its behavior under web.auth.api_keys_file: the forward
// surface may then answer 401, which is reported as the exchange status, not
// hidden).
func (s *Service) Replay(ctx context.Context, id, providerName string) (appapi.ReplayResult, error) {
	if id == "" {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest, "request id is required")
	}
	if providerName == "" {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest, "provider is required")
	}
	queries := s.RequestLogQueries()
	if queries == nil {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest,
			"request_log is disabled — nothing to replay")
	}
	records, err := queries.Detail(id, "")
	if err != nil {
		return appapi.ReplayResult{}, err
	}
	if len(records) == 0 {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusNotFound,
			"no request log record for id "+id)
	}
	rec := records[0]
	// The guards below mirror the CLI's DoReplay one-to-one.
	if strings.HasPrefix(rec.RequestID, "shadow-") {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest,
			"record "+id+" is a shadow evaluation record — shadow records cannot be replayed")
	}
	if !strings.HasPrefix(rec.Path, "/v1/") {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("record %s path %q is not under /v1/ — cannot replay", id, rec.Path))
	}
	if rec.RequestBody == "" {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest,
			"record "+id+" has no captured request body")
	}
	if rec.RequestBodyTruncated() {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest,
			"record "+id+" request body was truncated at request_log.max_body_bytes — replay would send an incomplete request; raise max_body_bytes and recapture")
	}
	cfg := s.ports.Config()
	if cfg == nil {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusServiceUnavailable, "no config generation")
	}
	if _, ok := cfg.Providers[providerName]; !ok {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("unknown provider %q — available: %s", providerName, cfg.ProviderNames()))
	}
	path := rec.Path
	if path == "" {
		path = "/v1/responses"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+cfg.Listen+path, bytes.NewReader([]byte(rec.RequestBody)))
	if err != nil {
		return appapi.ReplayResult{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-mp-force-provider", providerName)
	client := &http.Client{Timeout: cfg.Scheduling.Timeout()}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return appapi.ReplayResult{}, fmt.Errorf("replay request failed: %w", err)
	}
	defer resp.Body.Close()
	// Cap + 1 byte decides truncation without buffering the whole stream.
	limited, err := io.ReadAll(io.LimitReader(resp.Body, replayBodyCap+1))
	if err != nil {
		return appapi.ReplayResult{}, fmt.Errorf("read replay response: %w", err)
	}
	truncated := len(limited) > replayBodyCap
	if truncated {
		limited = limited[:replayBodyCap]
	}
	return appapi.ReplayResult{
		Status:    resp.StatusCode,
		Body:      string(limited),
		Truncated: truncated,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// TestRoute probes every route target of one exposed model once, in
// scheduling (priority) order — the daemon twin of `model-proxy test
// <model>`. The provider implementation resolves parent-or-first-pooled-
// virtual (the forward path's credential binding) via the RouteProbeImpl
// port. Per-target failures are data (ok:false + reason), never aborts.
func (s *Service) TestRoute(ctx context.Context, model string) (appapi.RouteTestResult, error) {
	if model == "" {
		return appapi.RouteTestResult{}, appapi.NewHTTPError(http.StatusBadRequest, "model is required")
	}
	cfg, _ := s.ports.ProbeRuntime()
	if cfg == nil {
		return appapi.RouteTestResult{}, appapi.NewHTTPError(http.StatusServiceUnavailable, "no config generation")
	}
	targets, ok := routing.RouteTable(cfg)[model]
	if !ok || len(targets) == 0 {
		return appapi.RouteTestResult{}, appapi.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("no route for model %q — available routes: %s", model, cfg.RouteNames()))
	}
	sorted := append([]configdomain.RouteTarget(nil), targets...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })

	result := appapi.RouteTestResult{Model: model, Results: []appapi.RouteTestTarget{}}
	client := &http.Client{Timeout: cfg.Scheduling.Timeout()}
	for _, target := range sorted {
		entry := appapi.RouteTestTarget{Provider: target.Provider, Model: target.Model}
		provCfg, ok := cfg.Providers[target.Provider]
		if !ok {
			entry.Reason = "provider not in config"
			result.Results = append(result.Results, entry)
			continue
		}
		impl := s.ports.RouteProbeImpl(target.Provider)
		if impl == nil {
			entry.Reason = "provider not available (not logged in?)"
			result.Results = append(result.Results, entry)
			continue
		}
		r := probe.Exchange(ctx, client, provCfg, impl, target.Model)
		entry.OK = r.OK
		entry.HTTPStatus = r.Status
		entry.Reason = r.Reason
		entry.LatencyMs = r.Latency.Milliseconds()
		result.Results = append(result.Results, entry)
	}
	return result, nil
}

// PullModelsCatalog force-refreshes the models.dev metadata cache — the
// daemon twin of `model-proxy models pull`. The refreshed cache is consumed
// by the next reload/takeover; the runtime is not reloaded here.
func (s *Service) PullModelsCatalog(_ context.Context) (appapi.ModelsCatalogPull, error) {
	homeDir := s.homeDir()
	cat, err := configdomain.LoadModelsCatalog(homeDir, true)
	if err != nil {
		return appapi.ModelsCatalogPull{}, err
	}
	return appapi.ModelsCatalogPull{Status: "refreshed", Count: cat.Count(), ETag: cat.ETag()}, nil
}
