package main

import (
	"context"
	"io"
	"model-proxy/internal/app"
	climodels "model-proxy/internal/cli/models"
	"model-proxy/internal/probe"
	"model-proxy/internal/protocol"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"model-proxy/internal/catalog"
	"model-proxy/provider"
)

// models_check_test.go covers the endpoint probe (models_check.go):
// probeModelCallable's per-protocol path/body/headers, error-body reason
// extraction, and checkProviderModels' kept/dropped split + stable order.

// fakeProviderImpl is a minimal provider.Provider for probe tests: AuthHeaders
// sets a marker Bearer, RewriteRequest records the path it received, ProbeRequest
// returns a fixed OpenAI-style probe (path/body). Used when the test wants to
// assert the probe FLOW (2xx/4xx/reason extraction) WITHOUT the full
// buildProviders auth wiring. Per-provider path/body assertions live in
// provider/probe_test.go (they test the real provider implementations directly).
type fakeProviderImpl struct {
	rewritePath string
	mu          sync.Mutex
}

func (f *fakeProviderImpl) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer TEST")
	return nil
}
func (f *fakeProviderImpl) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	f.mu.Lock()
	f.rewritePath = path
	f.mu.Unlock()
	return targetURL, body
}
func (f *fakeProviderImpl) Refresh() error                 { return nil }
func (f *fakeProviderImpl) Logout() error                  { return nil }
func (f *fakeProviderImpl) Usage() error                   { return nil }
func (f *fakeProviderImpl) FetchModels() ([]string, error) { return nil, nil }
func (f *fakeProviderImpl) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingUnknown}, nil
}
func (f *fakeProviderImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{
		Method: http.MethodPost,
		Path:   "/chat/completions",
		Body:   []byte(`{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1,"stream":false}`),
	}
}
func (f *fakeProviderImpl) ExtraHeaders(req *http.Request, path string)          {}
func (f *fakeProviderImpl) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

// readAll is a tiny test helper (io.ReadAll without the import noise at call sites).
func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}

// grabStderr captures everything fn writes to os.Stderr (used by print-filter
// tests, since printFilterSummary writes its summary to stderr).
func grabStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}

// --- probeModelCallable: openai 2xx = callable ---

func TestProbeModelCallable_OpenAI2xx(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	impl := &fakeProviderImpl{}
	prov := Provider{OpenAIBaseURL: srv.URL, Provider: "zhipu"}
	ok, status, reason := probe.Callable(context.Background(), srv.Client(), prov, impl, "glm-5.2")
	if !ok || status != 200 || reason != "" {
		t.Errorf("2xx probe: ok=%v status=%d reason=%q want ok=true,200,empty", ok, status, reason)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("openai probe path=%q want /chat/completions", gotPath)
	}
}

func TestProbeModelCallableContext_CancelsUpstreamRequest(t *testing.T) {
	started := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	client := &http.Client{Transport: modelProbeRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(upstreamCanceled)
		return nil, r.Context().Err()
	})}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var (
		ok     bool
		status int
		reason string
	)
	go func() {
		ok, status, reason = probe.Callable(
			ctx,
			client,
			Provider{OpenAIBaseURL: "https://probe.invalid", Provider: testProviderID},
			&fakeProviderImpl{},
			"cancel-me",
		)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not reach upstream")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe did not return after request context cancellation")
	}
	if ok || status != 0 || !strings.Contains(reason, context.Canceled.Error()) {
		t.Fatalf("cancelled probe = ok=%v status=%d reason=%q", ok, status, reason)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request context was not cancelled")
	}
}

type modelProbeRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f modelProbeRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// --- probeModelCallable: anthropic base → anthropic-shaped probe ---

// Regression: with anthropic_base_url set, the probe must switch BOTH base and
// shape (path /v1/messages + anthropic body + anthropic-version header) —
// probing {anthropic_base}/chat/completions 404s on strict bases (deepseek)
// and tests the wrong protocol on lenient ones (zhipu).
func TestProbeModelCallable_AnthropicBaseUsesAnthropicShape(t *testing.T) {
	var gotPath, gotVersion, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	impl := &fakeProviderImpl{}
	prov := Provider{OpenAIBaseURL: "http://openai-unused", AnthropicBaseURL: srv.URL, Provider: "deepseek"}
	ok, _, _ := probe.Callable(context.Background(), srv.Client(), prov, impl, "deepseek-v4-pro")
	if !ok {
		t.Error("anthropic-base probe: ok=false want true")
	}
	if gotPath != "/v1/messages" {
		t.Errorf("probe path=%q want /v1/messages", gotPath)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header missing")
	}
	if !strings.Contains(gotBody, `"messages"`) || !strings.Contains(gotBody, `"deepseek-v4-pro"`) {
		t.Errorf("probe body not anthropic-shaped: %s", gotBody)
	}
}

// --- probeModelCallable: 404 with error body -> reason extracted ---

func TestProbeModelCallable_404Reason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"code":"UnsupportedModel","message":"does not support the agent plan feature"}}`))
	}))
	defer srv.Close()

	impl := &fakeProviderImpl{}
	prov := Provider{OpenAIBaseURL: srv.URL, Provider: "volcengine"}
	ok, status, reason := probe.Callable(context.Background(), srv.Client(), prov, impl, "doubao-seedance-1.5-pro")
	if ok {
		t.Errorf("404 probe: ok=true want false")
	}
	if status != 404 {
		t.Errorf("404 probe: status=%d want 404", status)
	}
	if !strings.Contains(reason, "UnsupportedModel") || !strings.Contains(reason, "agent plan") {
		t.Errorf("404 reason=%q want code+message extracted", reason)
	}
}

// --- probeModelCallable: 500 with non-JSON body -> raw body fallback ---

func TestProbeModelCallable_500RawBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`InternalServiceError`))
	}))
	defer srv.Close()

	impl := &fakeProviderImpl{}
	prov := Provider{OpenAIBaseURL: srv.URL, Provider: "volcengine"}
	ok, status, reason := probe.Callable(context.Background(), srv.Client(), prov, impl, "doubao-embedding-vision")
	if ok || status != 500 {
		t.Errorf("500 probe: ok=%v status=%d want false,500", ok, status)
	}
	if !strings.Contains(reason, "InternalServiceError") {
		t.Errorf("500 reason=%q want raw body excerpt", reason)
	}
}

// --- probeModelCallable: anthropic path + codex /responses path ---
//
// These per-provider probe path/body/header assertions moved to
// provider/probe_test.go (TestAqpProbeRequest / TestCodexProbeRequest /
// TestDefaultProbeRequest / TestAqpExtraHeaders) - the probe shape is now owned
// by each provider's ProbeRequest/ExtraHeaders implementation, so the tests
// assert those methods directly (no HTTP server needed). The tests below cover
// the probe FLOW (2xx callable / 4xx reason extraction / 5xx raw body) with a
// fake impl that returns a fixed OpenAI-style probe.

// --- checkProviderModels: kept/dropped split + stable order + reasons ---

func TestCheckProviderModels_KeptDroppedOrder(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY")

	// Map model -> status. Ordering of ids passed in must be preserved in output.
	behaviors := map[string]int{
		"keep-a": 200,
		"drop-b": 404,
		"keep-c": 200,
		"drop-d": 500,
		"keep-e": 200,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// extract model from body
		body := readAll(r.Body)
		model := protocol.ExtractModel(body)
		code, ok := behaviors[model]
		if !ok {
			code = 404
		}
		w.WriteHeader(code)
		if code != 200 {
			w.Write([]byte(`{"error":{"code":"Bad","message":"nope"}}`))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"},
		},
	}
	ids := []string{"keep-a", "drop-b", "keep-c", "drop-d", "keep-e"}
	kept, dropped, err := climodels.CheckProviderModels(cfg, "zhipu", ids)
	if err != nil {
		t.Fatalf("checkProviderModels: %v", err)
	}
	wantKept := []string{"keep-a", "keep-c", "keep-e"}
	if len(kept) != len(wantKept) {
		t.Fatalf("kept=%v want %v", kept, wantKept)
	}
	for i, m := range wantKept {
		if kept[i] != m {
			t.Errorf("kept[%d]=%q want %q (order must be stable)", i, kept[i], m)
		}
	}
	wantDropped := []string{"drop-b", "drop-d"}
	if len(dropped) != len(wantDropped) {
		t.Fatalf("dropped=%+v want %v", dropped, wantDropped)
	}
	for i, m := range wantDropped {
		if dropped[i].Model != m {
			t.Errorf("dropped[%d].Model=%q want %q", i, dropped[i].Model, m)
		}
		if dropped[i].Reason == "" {
			t.Errorf("dropped[%d] (%s) reason empty", i, m)
		}
	}
}

// --- checkProviderModels: not logged in -> all dropped with auth reason (no error) ---
//
// buildProviders always builds a file-backed zhipu impl (ApiKeyBase reads the
// key lazily), so a not-logged-in provider surfaces as a probe failure (auth
// error), not a checkProviderModels error. The caller's fallback path is
// triggered only when buildProviders itself can't return an impl.

func TestCheckProviderModels_NotLoggedInAllDropped(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir) // no pool file, no singular file -> LoadKey will fail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"},
		},
	}
	kept, dropped, err := climodels.CheckProviderModels(cfg, "zhipu", []string{"glm-5.2"})
	if err != nil {
		t.Fatalf("not-logged-in: want no error (impl builds file-backed), got %v", err)
	}
	if len(kept) != 0 {
		t.Errorf("not-logged-in: kept=%v want empty (auth fails)", kept)
	}
	if len(dropped) != 1 || dropped[0].Model != "glm-5.2" {
		t.Errorf("not-logged-in: dropped=%+v want [glm-5.2]", dropped)
	}
	if dropped[0].Status != 0 || !strings.Contains(dropped[0].Reason, "auth") {
		t.Errorf("not-logged-in: dropped reason=%q status=%d want auth error / status 0", dropped[0].Reason, dropped[0].Status)
	}
}

// --- helpers: mergeModelIDs / sameStringSet / diffStringSets ---

func TestMergeModelIDs_DedupOrder(t *testing.T) {
	existing := []string{"a", "b"}
	entries := []climodels.ModelEntry{{ID: "b"}, {ID: "c"}, {ID: "a"}}
	got := climodels.MergeModelIDs(existing, entries)
	want := []string{"a", "b", "c"}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("mergeModelIDs=%v want %v (existing-first, deduped)", got, want)
	}
}

// --- mergeStringIDs: dedup, a-first-then-b order (shared by refresh + fallback) ---

func TestMergeStringIDs(t *testing.T) {
	got := climodels.MergeStringIDs([]string{"a", "b", "c"}, []string{"b", "d", "a", "e"})
	want := []string{"a", "b", "c", "d", "e"}
	if len(got) != len(want) {
		t.Fatalf("mergeStringIDs=%v want %v", got, want)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("mergeStringIDs[%d]=%q want %q (a-first, deduped)", i, got[i], s)
		}
	}
	// empty sides pass through cleanly.
	if got := climodels.MergeStringIDs(nil, []string{"x"}); len(got) != 1 || got[0] != "x" {
		t.Errorf("climodels.MergeStringIDs(nil,[x])=%v want [x]", got)
	}
	if got := climodels.MergeStringIDs([]string{"x"}, nil); len(got) != 1 || got[0] != "x" {
		t.Errorf("climodels.MergeStringIDs([x],nil)=%v want [x]", got)
	}
}

// --- routeModelsForProvider: route-target models for one provider, deduped+sorted ---

func TestRouteModelsForProvider(t *testing.T) {
	cfg := &Config{Routes: map[string][]RouteTarget{
		"glm-5.2":           {{Provider: "zhipu", Model: "glm-5.2"}, {Provider: "aqp", Model: "glm-5.2"}},
		"deepseek-v4-pro":   {{Provider: "aqp", Model: "deepseek-v4-pro"}, {Provider: "deepseek", Model: "deepseek-v4-pro"}},
		"deepseek-v4-flash": {{Provider: "aqp", Model: "deepseek-v4-flash"}},
		"gpt-5.5":           {{Provider: "codex", Model: "gpt-5.5"}},
	}}
	// aqp is targeted by 3 distinct models across routes; glm-5.2 appears in
	// two routes but must be deduped. Sorted for deterministic order.
	got := climodels.RouteModelsForProvider(cfg, "aqp")
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2"}
	if len(got) != len(want) {
		t.Fatalf("climodels.RouteModelsForProvider(aqp)=%v want %v", got, want)
	}
	for i, m := range want {
		if got[i] != m {
			t.Errorf("climodels.RouteModelsForProvider(aqp)[%d]=%q want %q (sorted, deduped)", i, got[i], m)
		}
	}
	// A provider not targeted by any route -> empty (no panic).
	if got := climodels.RouteModelsForProvider(cfg, "volcengine"); len(got) != 0 {
		t.Errorf("climodels.RouteModelsForProvider(volcengine)=%v want empty", got)
	}
	// No routes at all -> empty.
	if got := climodels.RouteModelsForProvider(&Config{}, "aqp"); len(got) != 0 {
		t.Errorf("climodels.RouteModelsForProvider(no-routes)=%v want empty", got)
	}
}

func TestSameStringSet(t *testing.T) {
	if !climodels.SameStringSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Errorf("same set {a,b}=={b,a} want true")
	}
	if climodels.SameStringSet([]string{"a", "b"}, []string{"a", "c"}) {
		t.Errorf("different sets want false")
	}
	if climodels.SameStringSet([]string{"a"}, []string{"a", "b"}) {
		t.Errorf("different sizes want false")
	}
}

func TestDiffStringSets(t *testing.T) {
	added, removed := climodels.DiffStringSets([]string{"a", "b"}, []string{"b", "c"})
	if len(added) != 1 || added[0] != "c" {
		t.Errorf("added=%v want [c]", added)
	}
	if len(removed) != 1 || removed[0] != "a" {
		t.Errorf("removed=%v want [a]", removed)
	}
}

// --- applyProviderModelFilter: volcengine policy pass ---

// --- applyProviderModelFilter: volcengine policy pass moved to
// provider/probe_test.go (TestVolcengineFilterModelIDs / TestDefaultFilterPassthrough),
// testing the real VolcengineProvider.FilterModelIDs override directly. ---

// --- stripRequestID: trims trailing "Request id: <hex>" noise ---

func TestStripRequestID(t *testing.T) {
	in := "AccessDenied: does not have access to messages api Request id: 021783844191650eb717b7491a66ffdd25ddc462b95700bc69454"
	want := "AccessDenied: does not have access to messages api"
	if got := probe.StripRequestID(in); got != want {
		t.Errorf("stripRequestID=%q want %q", got, want)
	}
	// no request id -> unchanged
	if got := probe.StripRequestID("plain error"); got != "plain error" {
		t.Errorf("probe.StripRequestID(no-id)=%q want unchanged", got)
	}
	// case-insensitive
	if got := probe.StripRequestID("err REQUEST ID: abc"); got != "err" {
		t.Errorf("probe.StripRequestID(upper)=%q want 'err'", got)
	}
}

func TestPrintKeptModels(t *testing.T) {
	// models.dev metadata for two kept models: one with full metadata, one default.
	meta := map[string]map[string]catalog.Model{
		"volcengine": {
			"glm-5.2":   {Context: 200000, Output: 16384, Modalities: catalog.Modalities{Input: []string{"text", "image"}}},
			"kimi-k2.6": {}, // no metadata -> defaults (text, "-")
		},
	}
	sources := map[string]map[string]app.ModelSource{
		"volcengine": {"glm-5.2": app.SrcModelsDev, "kimi-k2.6": app.SrcDefault},
	}
	out := grabStdout(t, func() { climodels.PrintKeptModels("volcengine", []string{"glm-5.2", "kimi-k2.6"}, meta, sources) })
	if !strings.Contains(out, "glm-5.2") || !strings.Contains(out, "kimi-k2.6") {
		t.Errorf("printKeptModels missing models: %q", out)
	}
	if !strings.Contains(out, "provider: volcengine: 2 models") {
		t.Errorf("printKeptModels missing count line: %q", out)
	}
	// Rich metadata columns rendered: context, output, input modalities, source.
	if !strings.Contains(out, "200000") {
		t.Errorf("printKeptModels missing CTX=200000: %q", out)
	}
	if !strings.Contains(out, "16384") {
		t.Errorf("printKeptModels missing OUTPUT=16384: %q", out)
	}
	if !strings.Contains(out, "text/image") {
		t.Errorf("printKeptModels missing INPUT MODALITIES=text/image: %q", out)
	}
	if !strings.Contains(out, "models.dev") {
		t.Errorf("printKeptModels missing SRC=models.dev: %q", out)
	}
}

func TestPrintKeptModels_Empty(t *testing.T) {
	out := grabStdout(t, func() { climodels.PrintKeptModels("x", nil, nil, nil) })
	if !strings.Contains(out, "(no models)") {
		t.Errorf("empty printKeptModels=%q want (no models)", out)
	}
}

// --- printFilterSummary: drops with reasons on stderr, policy vs probe ---

func TestPrintFilterSummary_PolicyAndProbe(t *testing.T) {
	policyDropped := []string{"glm-latest", "kimi-latest"}
	probeDropped := []climodels.DropReason{
		{Model: "doubao-seedance-2.0", Status: 403, Reason: "AccessDenied: does not have access to messages api"},
		{Model: "doubao-seedream-5.0-lite", Status: 403, Reason: "AccessDenied: does not have access to messages api"},
	}
	out := grabStderr(t, func() { climodels.PrintFilterSummary(policyDropped, probeDropped, nil, false) })
	if !strings.Contains(out, "filtered out 4 model(s)") {
		t.Errorf("summary count: %q want 'filtered out 4 model(s)'", out)
	}
	if !strings.Contains(out, "glm-latest") || !strings.Contains(out, "excluded by filter rule") {
		t.Errorf("summary missing policy drop + reason: %q", out)
	}
	if !strings.Contains(out, "doubao-seedance-2.0") || !strings.Contains(out, "not callable on base_url") {
		t.Errorf("summary missing probe drop + reason: %q", out)
	}
	if !strings.Contains(out, "AccessDenied") {
		t.Errorf("summary missing upstream error detail: %q", out)
	}
}

func TestPrintFilterSummary_NoDrops(t *testing.T) {
	out := grabStderr(t, func() { climodels.PrintFilterSummary(nil, nil, nil, false) })
	if out != "" {
		t.Errorf("no-drops summary=%q want empty", out)
	}
}

func TestPrintFilterSummary_AllFailedWarning(t *testing.T) {
	probeDropped := []climodels.DropReason{{Model: "glm-5.2", Status: 0, Reason: "auth: no key"}}
	out := grabStderr(t, func() { climodels.PrintFilterSummary(nil, probeDropped, nil, true) })
	if !strings.Contains(out, "failed for ALL") {
		t.Errorf("all-failed summary=%q want 'failed for ALL' warning", out)
	}
	if !strings.Contains(out, "login/network") {
		t.Errorf("all-failed summary=%q want login/network hint", out)
	}
}
