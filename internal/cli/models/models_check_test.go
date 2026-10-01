package models

import (
	"context"
	"io"
	"model-proxy/internal/cli/clitest"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/probe"
	"model-proxy/internal/protocol"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"model-proxy/internal/catalog"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/routing"
	runtimewire "model-proxy/internal/runtime/wirecap"
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
func (f *fakeProviderImpl) ExtraHeaders(req *http.Request, _ []byte, _ string, path string) {}
func (f *fakeProviderImpl) FilterModelIDs(ids []string) (kept, dropped []string)            { return ids, nil }

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
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL, Provider: "zhipu"}
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
			configdomain.Provider{OpenAIBaseURL: "https://probe.invalid", Provider: "static"},
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
	prov := configdomain.Provider{OpenAIBaseURL: "http://openai-unused", AnthropicBaseURL: srv.URL, Provider: "deepseek"}
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
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL, Provider: "volcengine"}
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
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL, Provider: "volcengine"}
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

// --- checkProviderModels: 3-leg kept/dropped split + stable order + reasons + caps persist ---

func TestCheckProviderModels_KeptDroppedOrder(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "KEY")

	// Per (path, model) status table. The 3-leg probe hits /chat/completions and
	// /responses on the openai base (the anthropic leg is unprobed - no
	// anthropic_base_url). A model is KEPT when ANY leg classifies Yes (2xx
	// here), or when no PROBED leg classifies No: 404 -> No drops the model,
	// but 500 -> Unknown on every probed leg holds no negative information and
	// keeps it (a transient storm must not read as a deletion). Input order
	// must be preserved in the outputs.
	type legKey struct{ path, model string }
	statuses := map[legKey]int{
		{"/chat/completions", "keep-a"}:      200,
		{"/responses", "keep-a"}:             200,
		{"/chat/completions", "drop-b"}:      404,
		{"/responses", "drop-b"}:             404,
		{"/chat/completions", "resp-only-c"}: 404,
		{"/responses", "resp-only-c"}:        200,
		{"/chat/completions", "flaky-d"}:     500,
		{"/responses", "flaky-d"}:            500,
		{"/chat/completions", "keep-e"}:      200,
		{"/responses", "keep-e"}:             404,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := protocol.ExtractModel(readAll(r.Body))
		code, ok := statuses[legKey{r.URL.Path, model}]
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"},
		},
	}
	ids := []string{"keep-a", "drop-b", "resp-only-c", "flaky-d", "keep-e"}
	kept, dropped, protocols, capsPersisted, err := CheckProviderModels(cfg, "zhipu", ids, nil)
	if err != nil {
		t.Fatalf("checkProviderModels: %v", err)
	}
	if !capsPersisted {
		t.Error("capsPersisted = false, want true (fresh matrix landed on disk)")
	}
	wantKept := []string{"keep-a", "resp-only-c", "flaky-d", "keep-e"}
	if len(kept) != len(wantKept) {
		t.Fatalf("kept=%v want %v (any leg Yes, or no probed leg No)", kept, wantKept)
	}
	for i, m := range wantKept {
		if kept[i] != m {
			t.Errorf("kept[%d]=%q want %q (order must be stable)", i, kept[i], m)
		}
	}
	wantDropped := []string{"drop-b"}
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
	// drop-b: 404 on both probed legs -> per-leg summary with the chat status
	// surfaced, the anthropic leg reported as unprobed, and the upstream's
	// error code+message extracted per leg.
	if dropped[0].Status != 404 {
		t.Errorf("drop-b status=%d want 404 (chat leg's)", dropped[0].Status)
	}
	for _, want := range []string{"chat HTTP 404: Bad: nope", "anthropic not probed (no base)", "responses HTTP 404: Bad: nope"} {
		if !strings.Contains(dropped[0].Reason, want) {
			t.Errorf("drop-b reason=%q missing %q", dropped[0].Reason, want)
		}
	}
	// The returned matrix records the per-leg verdicts: resp-only-c is kept
	// BECAUSE the responses leg classified Yes despite the chat 404.
	mp, ok := protocols["resp-only-c"]
	if !ok {
		t.Fatalf("protocols missing resp-only-c: %v", protocols)
	}
	if mp.Chat != runtimewire.No || mp.Anthropic != runtimewire.No || mp.Responses != runtimewire.Yes {
		t.Errorf("resp-only-c matrix = chat:%s ant:%s resp:%s, want no/no/yes", mp.Chat, mp.Anthropic, mp.Responses)
	}
	// flaky-d is kept with every PROBED leg inconclusive (5xx -> unknown); the
	// anthropic leg is definitionally no (no base) and does not count as
	// negative evidence.
	if mp := protocols["flaky-d"]; mp.Chat != runtimewire.Unknown || mp.Responses != runtimewire.Unknown || mp.Anthropic != runtimewire.No {
		t.Errorf("flaky-d matrix = chat:%s ant:%s resp:%s, want unknown/no/unknown", mp.Chat, mp.Anthropic, mp.Responses)
	}
	// The fresh matrix was persisted to model_caps.json under the isolated HOME,
	// fingerprinted with the provider's current protocol config.
	loaded, err := runtimewire.LoadModelCapsFile(filepath.Join(dir, ".model-proxy", "model_caps.json"))
	if err != nil {
		t.Fatalf("LoadModelCapsFile: %v", err)
	}
	entry, ok := loaded["zhipu"]
	if !ok {
		t.Fatalf("model_caps.json missing zhipu entry: %v", loaded)
	}
	if want := providerbuild.ProtocolConfigFingerprint(cfg.Providers["zhipu"]); entry.Fingerprint != want {
		t.Errorf("caps fingerprint=%q want %q", entry.Fingerprint, want)
	}
	if len(entry.Models) != len(ids) {
		t.Errorf("caps models=%v want one entry per probed id (%d)", entry.Models, len(ids))
	}
	if got := entry.Models["resp-only-c"]; got.Responses != runtimewire.Yes {
		t.Errorf("persisted resp-only-c.responses=%s want yes", got.Responses)
	}
}

// --- checkProviderModels: not logged in -> every leg inconclusive, model kept ---
//
// buildProviders always builds a file-backed zhipu impl (ApiKeyBase reads the
// key lazily), so a not-logged-in provider surfaces as a probe failure (auth
// error), not a checkProviderModels error. The caller's fallback path is
// triggered only when buildProviders itself can't return an impl. Auth
// failures classify Unknown (no HTTP exchange), and a model with no probed
// negative leg is KEPT: a login outage holds no callability information and
// must not read as a config deletion (the allProbeFailed safety net used to
// catch exactly this sweep at the provider level; the per-model rule now
// covers it directly).

func TestCheckProviderModels_NotLoggedInKeepsInconclusiveModel(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir) // no pool file, no singular file -> LoadKey will fail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"},
		},
	}
	kept, dropped, protocols, _, err := CheckProviderModels(cfg, "zhipu", []string{"glm-5.2"}, nil)
	if err != nil {
		t.Fatalf("not-logged-in: want no error (impl builds file-backed), got %v", err)
	}
	if len(kept) != 1 || kept[0] != "glm-5.2" {
		t.Errorf("not-logged-in: kept=%v want [glm-5.2] (auth failures are inconclusive, not negative)", kept)
	}
	if len(dropped) != 0 {
		t.Errorf("not-logged-in: dropped=%+v want empty (no probed leg concluded No)", dropped)
	}
	// Every probed leg failed at the auth step (no HTTP exchange -> status 0)
	// and classifies Unknown; the anthropic leg is unprobed (no
	// anthropic_base_url configured) and definitionally No.
	if mp := protocols["glm-5.2"]; mp.Chat != runtimewire.Unknown || mp.Responses != runtimewire.Unknown || mp.Anthropic != runtimewire.No {
		t.Errorf("not-logged-in: matrix = chat:%s ant:%s resp:%s, want unknown/no/unknown", mp.Chat, mp.Anthropic, mp.Responses)
	}
}

// --- helpers: mergeModelIDs / sameStringSet / diffStringSets ---

func TestMergeModelIDs_DedupOrder(t *testing.T) {
	existing := []string{"a", "b"}
	entries := []ModelEntry{{ID: "b"}, {ID: "c"}, {ID: "a"}}
	got := MergeModelIDs(existing, entries)
	want := []string{"a", "b", "c"}
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("mergeModelIDs=%v want %v (existing-first, deduped)", got, want)
	}
}

// --- mergeStringIDs: dedup, a-first-then-b order (shared by refresh + fallback) ---

func TestMergeStringIDs(t *testing.T) {
	got := MergeStringIDs([]string{"a", "b", "c"}, []string{"b", "d", "a", "e"})
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
	if got := MergeStringIDs(nil, []string{"x"}); len(got) != 1 || got[0] != "x" {
		t.Errorf("MergeStringIDs(nil,[x])=%v want [x]", got)
	}
	if got := MergeStringIDs([]string{"x"}, nil); len(got) != 1 || got[0] != "x" {
		t.Errorf("MergeStringIDs([x],nil)=%v want [x]", got)
	}
}

// --- routeModelsForProvider: route-target models for one provider, deduped+sorted ---

func TestRouteModelsForProvider(t *testing.T) {
	cfg := &configdomain.Config{Routes: map[string][]configdomain.RouteTarget{
		"glm-5.2":           {{Provider: "zhipu", Model: "glm-5.2"}, {Provider: "aqp", Model: "glm-5.2"}},
		"deepseek-v4-pro":   {{Provider: "aqp", Model: "deepseek-v4-pro"}, {Provider: "deepseek", Model: "deepseek-v4-pro"}},
		"deepseek-v4-flash": {{Provider: "aqp", Model: "deepseek-v4-flash"}},
		"gpt-5.5":           {{Provider: "codex", Model: "gpt-5.5"}},
	}}
	// aqp is targeted by 3 distinct models across routes; glm-5.2 appears in
	// two routes but must be deduped. Sorted for deterministic order.
	got := routing.RouteModelsForProvider(cfg, "aqp")
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro", "glm-5.2"}
	if len(got) != len(want) {
		t.Fatalf("RouteModelsForProvider(aqp)=%v want %v", got, want)
	}
	for i, m := range want {
		if got[i] != m {
			t.Errorf("RouteModelsForProvider(aqp)[%d]=%q want %q (sorted, deduped)", i, got[i], m)
		}
	}
	// A provider not targeted by any route -> empty (no panic).
	if got := routing.RouteModelsForProvider(cfg, "volcengine"); len(got) != 0 {
		t.Errorf("RouteModelsForProvider(volcengine)=%v want empty", got)
	}
	// No routes at all -> empty.
	if got := routing.RouteModelsForProvider(&configdomain.Config{}, "aqp"); len(got) != 0 {
		t.Errorf("RouteModelsForProvider(no-routes)=%v want empty", got)
	}
}

func TestSameStringSet(t *testing.T) {
	if !SameStringSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Errorf("same set {a,b}=={b,a} want true")
	}
	if SameStringSet([]string{"a", "b"}, []string{"a", "c"}) {
		t.Errorf("different sets want false")
	}
	if SameStringSet([]string{"a"}, []string{"a", "b"}) {
		t.Errorf("different sizes want false")
	}
}

func TestDiffStringSets(t *testing.T) {
	added, removed := DiffStringSets([]string{"a", "b"}, []string{"b", "c"})
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
	sources := map[string]map[string]routing.ModelSource{
		"volcengine": {"glm-5.2": routing.SrcModelsDev, "kimi-k2.6": routing.SrcDefault},
	}
	protocols := map[string]map[string]runtimewire.ModelProtocols{
		"volcengine": {
			"glm-5.2": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Yes},
			// kimi-k2.6 has no entry -> "-"
		},
	}
	// Upstream display names: the NAME column prefers the live upstream name —
	// an upstream can swap the model behind a stable id (Kimi Code serves
	// "K2.8 Preview" as `kimi-for-coding`), and the upstream's own name is the
	// only signal that surfaces the swap.
	upstream := map[string]string{"kimi-k2.6": "K2.8 Preview"}
	out := clitest.GrabStdout(t, func() {
		PrintKeptModels("volcengine", []string{"glm-5.2", "kimi-k2.6"}, meta, sources, protocols, upstream)
	})
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
	// PROTOCOLS column: header, the Yes legs in chat/ant/resp order, "-" for the
	// model without an entry.
	if !strings.Contains(out, "PROTOCOLS") {
		t.Errorf("printKeptModels missing PROTOCOLS header: %q", out)
	}
	if !strings.Contains(out, "chat/resp") {
		t.Errorf("printKeptModels missing PROTOCOLS=chat/resp for glm-5.2: %q", out)
	}
	// NAME column: upstream display name wins over the id fallback.
	if !strings.Contains(out, "K2.8 Preview") {
		t.Errorf("printKeptModels missing upstream display name: %q", out)
	}
}

func TestPrintKeptModels_Empty(t *testing.T) {
	out := clitest.GrabStdout(t, func() { PrintKeptModels("x", nil, nil, nil, nil, nil) })
	if !strings.Contains(out, "(no models)") {
		t.Errorf("empty printKeptModels=%q want (no models)", out)
	}
}

// --- printFilterSummary: drops with reasons on stderr, policy vs probe ---

func TestPrintFilterSummary_PolicyAndProbe(t *testing.T) {
	policyDropped := []string{"glm-latest", "kimi-latest"}
	probeDropped := []DropReason{
		{Model: "doubao-seedance-2.0", Status: 403, Reason: "AccessDenied: does not have access to messages api"},
		{Model: "doubao-seedream-5.0-lite", Status: 403, Reason: "AccessDenied: does not have access to messages api"},
	}
	out := grabStderr(t, func() { PrintFilterSummary(policyDropped, probeDropped, nil, false) })
	if !strings.Contains(out, "filtered out 4 model(s)") {
		t.Errorf("summary count: %q want 'filtered out 4 model(s)'", out)
	}
	if !strings.Contains(out, "glm-latest") || !strings.Contains(out, "excluded by filter rule") {
		t.Errorf("summary missing policy drop + reason: %q", out)
	}
	if !strings.Contains(out, "doubao-seedance-2.0") || !strings.Contains(out, "not callable on any protocol") {
		t.Errorf("summary missing probe drop + reason: %q", out)
	}
	if !strings.Contains(out, "AccessDenied") {
		t.Errorf("summary missing upstream error detail: %q", out)
	}
}

func TestPrintFilterSummary_NoDrops(t *testing.T) {
	out := grabStderr(t, func() { PrintFilterSummary(nil, nil, nil, false) })
	if out != "" {
		t.Errorf("no-drops summary=%q want empty", out)
	}
}

func TestPrintFilterSummary_AllFailedWarning(t *testing.T) {
	probeDropped := []DropReason{{Model: "glm-5.2", Status: 0, Reason: "auth: no key"}}
	out := grabStderr(t, func() { PrintFilterSummary(nil, probeDropped, nil, true) })
	if !strings.Contains(out, "failed for ALL") {
		t.Errorf("all-failed summary=%q want 'failed for ALL' warning", out)
	}
	if !strings.Contains(out, "login/network") {
		t.Errorf("all-failed summary=%q want login/network hint", out)
	}
}

// --- protocolsCell: PROTOCOLS column rendering ---

func TestProtocolsCell(t *testing.T) {
	cases := []struct {
		name string
		mp   runtimewire.ModelProtocols
		ok   bool
		want string
	}{
		{"no entry", runtimewire.ModelProtocols{}, false, "-"},
		{"chat+responses yes", runtimewire.ModelProtocols{Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Yes}, true, "chat/resp"},
		{"all three yes in chat/ant/resp order", runtimewire.ModelProtocols{Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.Yes}, true, "chat/ant/resp"},
		{"anthropic only", runtimewire.ModelProtocols{Chat: runtimewire.No, Anthropic: runtimewire.Yes, Responses: runtimewire.No}, true, "ant"},
		{"all no", runtimewire.ModelProtocols{Chat: runtimewire.No, Anthropic: runtimewire.No, Responses: runtimewire.No}, true, "none"},
		{"all unknown", runtimewire.ModelProtocols{}, true, "-"},
		{"mixed no+unknown, no yes", runtimewire.ModelProtocols{Chat: runtimewire.No, Anthropic: runtimewire.Unknown, Responses: runtimewire.Unknown}, true, "-"},
	}
	for _, tc := range cases {
		if got := protocolsCell(tc.mp, tc.ok); got != tc.want {
			t.Errorf("%s: protocolsCell=%q want %q", tc.name, got, tc.want)
		}
	}
}

// --- loadModelCapsProjection: fingerprint-gated read of model_caps.json ---

func TestLoadModelCapsProjection(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://o"},
		"aqp":   {Provider: "aqp", OpenAIBaseURL: "https://a"},
	}}
	matrix := map[string]runtimewire.ModelProtocols{
		"glm-5.2": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Yes},
	}
	capsPath := filepath.Join(dir, ".model-proxy", "model_caps.json")
	err := runtimewire.SaveModelCapsFile(capsPath, map[string]runtimewire.ProviderModelCaps{
		// Matches the current zhipu config -> projected.
		"zhipu": {Fingerprint: providerbuild.ProtocolConfigFingerprint(cfg.Providers["zhipu"]), ProbedAt: time.Now(), Models: matrix},
		// Stale fingerprint (different base url) -> dropped.
		"aqp": {Fingerprint: providerbuild.ProtocolConfigFingerprint(configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://OLD"}), ProbedAt: time.Now(), Models: matrix},
		// Provider no longer in config -> dropped.
		"gone": {Fingerprint: "whatever", ProbedAt: time.Now(), Models: matrix},
	})
	if err != nil {
		t.Fatal(err)
	}

	proj := loadModelCapsProjection(cfg)
	if len(proj) != 1 {
		t.Fatalf("projection providers=%v want only [zhipu]", proj)
	}
	if got := proj["zhipu"]["glm-5.2"]; got != matrix["glm-5.2"] {
		t.Errorf("projection zhipu/glm-5.2=%+v want %+v", got, matrix["glm-5.2"])
	}

	// Missing file -> nil, no error surfaced.
	clitest.SetPoolHome(t, t.TempDir())
	if got := loadModelCapsProjection(cfg); got != nil {
		t.Errorf("missing file: projection=%v want nil", got)
	}

	// Malformed file -> nil, no error surfaced.
	dir2 := t.TempDir()
	clitest.SetPoolHome(t, dir2)
	bad := filepath.Join(dir2, ".model-proxy", "model_caps.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadModelCapsProjection(cfg); got != nil {
		t.Errorf("malformed file: projection=%v want nil", got)
	}
}

// TestPersistModelCaps_UnknownRetainsConcludedFileVerdict: `models refresh`
// persisting a partially rate-limited matrix must not regress verdicts already
// concluded in model_caps.json under the same fingerprint — an Unknown leg is
// "no information", so the stored yes/no wins (the CLI-side twin of
// wirecap.ModelStore.Put's merge; fixes the zcode/zhipu "? unknown" flapping
// where a refresh burst of 429s downgraded known-good legs on disk).
func TestPersistModelCaps_UnknownRetainsConcludedFileVerdict(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)

	provCfg := configdomain.Provider{OpenAIBaseURL: "https://example.test/v1", Provider: "zcode"}
	fp := providerbuild.ProtocolConfigFingerprint(provCfg)

	// Prior refresh concluded glm chat=yes / anthropic=yes / responses=no.
	if err := runtimewire.SaveModelCapsFile(filepath.Join(dir, ".model-proxy", "model_caps.json"),
		map[string]runtimewire.ProviderModelCaps{
			"zcode": {Fingerprint: fp, ProbedAt: time.Now(), Models: map[string]runtimewire.ModelProtocols{
				"glm": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.No},
			}},
		}); err != nil {
		t.Fatal(err)
	}

	// A throttled refresh: every leg 429 → unknown.
	if !persistModelCaps("zcode", provCfg, map[string]runtimewire.ModelProtocols{
		"glm": {Chat: runtimewire.Unknown, Anthropic: runtimewire.Unknown, Responses: runtimewire.Unknown},
	}, nil) {
		t.Fatal("persistModelCaps reported no write")
	}
	loaded, err := runtimewire.LoadModelCapsFile(filepath.Join(dir, ".model-proxy", "model_caps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["zcode"].Models["glm"]; got.Chat != runtimewire.Yes || got.Anthropic != runtimewire.Yes || got.Responses != runtimewire.No {
		t.Errorf("after throttled refresh glm = %+v, want prior yes/yes/no retained", got)
	}

	// A concluded re-probe still overwrites (corrections are not blocked).
	if !persistModelCaps("zcode", provCfg, map[string]runtimewire.ModelProtocols{
		"glm": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Unknown},
	}, nil) {
		t.Fatal("persistModelCaps reported no write")
	}
	loaded, err = runtimewire.LoadModelCapsFile(filepath.Join(dir, ".model-proxy", "model_caps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["zcode"].Models["glm"]; got.Anthropic != runtimewire.No || got.Responses != runtimewire.No {
		t.Errorf("after concluded re-probe glm = %+v, want anthropic no (correction lands) + responses no (retained)", got)
	}

	// A different fingerprint (config changed bases) is a different endpoint's
	// truth: no merge, unknowns land as-is.
	other := provCfg
	other.OpenAIBaseURL = "https://other.test/v1"
	if !persistModelCaps("zcode", other, map[string]runtimewire.ModelProtocols{
		"glm": {Chat: runtimewire.Unknown, Anthropic: runtimewire.Unknown, Responses: runtimewire.Unknown},
	}, nil) {
		t.Fatal("persistModelCaps reported no write")
	}
	loaded, err = runtimewire.LoadModelCapsFile(filepath.Join(dir, ".model-proxy", "model_caps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["zcode"].Models["glm"]; got.Chat != runtimewire.Unknown {
		t.Errorf("after fingerprint change glm = %+v, want fresh unknowns (no cross-fingerprint merge)", got)
	}
}

// TestCheckProviderModels_RateLimitedModelNotDropped (#4 regression): among
// several candidates, the ONE model whose every probed leg drew a transient
// 429 must be KEPT — the probe holds no negative information about it, and
// dropping it would write the rate-limit storm back as a config deletion
// (the allProbeFailed safety net only fires when EVERY model fails, so it
// never protected the single throttled model).
func TestCheckProviderModels_RateLimitedModelNotDropped(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "KEY")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := protocol.ExtractModel(readAll(r.Body))
		if model == "throttled" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"1302","message":"Concurrency limit reached"}}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"},
		},
	}
	kept, dropped, protocols, _, err := CheckProviderModels(cfg, "zhipu", []string{"ok-1", "throttled", "ok-2"}, nil)
	if err != nil {
		t.Fatalf("checkProviderModels: %v", err)
	}
	if !reflect.DeepEqual(kept, []string{"ok-1", "throttled", "ok-2"}) {
		t.Errorf("kept = %v, want all three (the 429-only model is inconclusive, not uncallable)", kept)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %+v, want empty (no probed leg concluded No)", dropped)
	}
	if mp := protocols["throttled"]; mp.Chat != runtimewire.Unknown || mp.Responses != runtimewire.Unknown {
		t.Errorf("throttled matrix = chat:%s resp:%s, want unknown/unknown", mp.Chat, mp.Responses)
	}
}

// TestPersistModelCaps_ReMergeOnConcurrentWrite (#8 regression): the CLI's
// whole-file read-modify-write races the daemon's async persist. When another
// writer lands new verdicts (here: provider "daemon-prov") between the CLI's
// read and its write, the mtime guard must detect the conflict and re-load /
// re-merge onto the fresher base — the final file must carry BOTH writers'
// data instead of the CLI's stale snapshot overwriting the daemon's.
func TestPersistModelCaps_ReMergeOnConcurrentWrite(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)

	provCfg := configdomain.Provider{OpenAIBaseURL: "https://example.test/v1", Provider: "zcode"}
	fp := providerbuild.ProtocolConfigFingerprint(provCfg)
	capsPath := filepath.Join(dir, ".model-proxy", "model_caps.json")

	// The CLI's pre-probe world: only zcode's old entry exists.
	if err := runtimewire.SaveModelCapsFile(capsPath, map[string]runtimewire.ProviderModelCaps{
		"zcode": {Fingerprint: fp, ProbedAt: time.Now(), Models: map[string]runtimewire.ModelProtocols{
			"glm": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.No},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Land the daemon's write at the exact read→write race point (first
	// attempt only; the retry must observe it on the re-load).
	var once sync.Once
	persistConflictSeam = func(path string) {
		once.Do(func() {
			loaded, err := runtimewire.LoadModelCapsFile(path)
			if err != nil {
				t.Errorf("seam load: %v", err)
				return
			}
			loaded["daemon-prov"] = runtimewire.ProviderModelCaps{
				Fingerprint: "fp-daemon", ProbedAt: time.Now(),
				Models: map[string]runtimewire.ModelProtocols{"m9": {Chat: runtimewire.Yes}},
			}
			if err := runtimewire.SaveModelCapsFile(path, loaded); err != nil {
				t.Errorf("seam write: %v", err)
			}
		})
	}
	t.Cleanup(func() { persistConflictSeam = nil })

	if !persistModelCaps("zcode", provCfg, map[string]runtimewire.ModelProtocols{
		"glm": {Chat: runtimewire.Yes, Anthropic: runtimewire.Unknown, Responses: runtimewire.Yes},
	}, nil) {
		t.Fatal("persistModelCaps reported no write")
	}

	loaded, err := runtimewire.LoadModelCapsFile(capsPath)
	if err != nil {
		t.Fatal(err)
	}
	// The daemon's mid-write verdicts survived the CLI's persist.
	if got, ok := loaded["daemon-prov"].Models["m9"]; !ok || got.Chat != runtimewire.Yes {
		t.Errorf("daemon-prov entry = %+v (ok=%v), want preserved — the CLI's stale snapshot overwrote the concurrent write", got, ok)
	}
	// And the CLI's own fresh matrix landed (unknown anthropic merged back to
	// the stored yes).
	if got := loaded["zcode"].Models["glm"]; got.Chat != runtimewire.Yes || got.Anthropic != runtimewire.Yes || got.Responses != runtimewire.Yes {
		t.Errorf("zcode glm = %+v, want yes/yes(yes merged)/yes", got)
	}
}

// TestCheckProviderModels_DisabledNotProbed: `models refresh` sends no probe
// request for operator-disabled models, keeps them in the list regardless of
// callability, and preserves their stored verdicts in model_caps.json (not
// probed ≠ dropped). The all-failed safety net must still fire on the probed
// subset (disabled ids cannot mask a total probe outage).
func TestCheckProviderModels_DisabledNotProbed(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "KEY")

	var hits sync.Map // model -> count
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := protocol.ExtractModel(readAll(r.Body))
		n, _ := hits.LoadOrStore(model, 0)
		hits.Store(model, n.(int)+1)
		if model == "dead" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// Disabled model carries a prior verdict in the file.
	provCfg := configdomain.Provider{OpenAIBaseURL: srv.URL, Provider: "zhipu"}
	fp := providerbuild.ProtocolConfigFingerprint(provCfg)
	if err := runtimewire.SaveModelCapsFile(filepath.Join(dir, ".model-proxy", "model_caps.json"),
		map[string]runtimewire.ProviderModelCaps{
			"zhipu": {Fingerprint: fp, ProbedAt: time.Now(), Models: map[string]runtimewire.ModelProtocols{
				"off": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.No},
			}},
		}); err != nil {
		t.Fatal(err)
	}

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": provCfg}}
	kept, dropped, protocols, _, err := CheckProviderModels(cfg, "zhipu", []string{"live", "off", "dead"}, []string{"off"})
	if err != nil {
		t.Fatalf("checkProviderModels: %v", err)
	}
	if got := kept; len(got) != 1 || got[0] != "live" {
		t.Errorf("probed kept = %v, want [live] only (disabled ids are the caller's to append)", got)
	}
	if len(dropped) != 1 || dropped[0].Model != "dead" {
		t.Errorf("dropped = %+v, want only dead", dropped)
	}
	if _, probed := protocols["off"]; probed {
		t.Error("disabled model leaked into the fresh matrix")
	}
	if n, _ := hits.Load("off"); n != nil && n.(int) != 0 {
		t.Errorf("disabled model was probed %d times", n)
	}
	for _, m := range []string{"live", "dead"} {
		if n, _ := hits.Load(m); n == nil || n.(int) == 0 {
			t.Errorf("model %s not probed", m)
		}
	}
	// The file entry preserved the disabled model's frozen verdicts.
	loaded, err := runtimewire.LoadModelCapsFile(filepath.Join(dir, ".model-proxy", "model_caps.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["zhipu"].Models["off"]; got.Chat != runtimewire.Yes || got.Anthropic != runtimewire.Yes {
		t.Errorf("disabled model's stored verdict = %+v, want carried over yes/yes", got)
	}
	// Every PROBED id lands in the file matrix (config dropping a model
	// never erased its verdict entry — same rule as before), while the
	// disabled id rode along unprobed.
	if _, ok := loaded["zhipu"].Models["dead"]; !ok {
		t.Error("probed-but-dropped model lost its file entry")
	}
}

// TestSplitDisabledModelIDs: the partition reads the operator override file
// under the isolated HOME; input order is preserved in both outputs; a
// missing file degrades to probing everything.
func TestSplitDisabledModelIDs(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	if err := runtimewire.SaveDisabledModelsFile(filepath.Join(dir, ".model-proxy", "disabled_models.json"),
		map[string][]string{"zhipu": {"off-a", "off-b"}}); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"zhipu": {Provider: "zhipu"},
	}}
	probeIDs, disabledIDs := SplitDisabledModelIDs(cfg, "zhipu", []string{"on-a", "off-a", "on-b", "off-b"})
	if !reflect.DeepEqual(probeIDs, []string{"on-a", "on-b"}) || !reflect.DeepEqual(disabledIDs, []string{"off-a", "off-b"}) {
		t.Errorf("partition = probe %v disabled %v", probeIDs, disabledIDs)
	}
	// Another provider is unaffected.
	probeIDs, disabledIDs = SplitDisabledModelIDs(cfg, "other", []string{"off-a"})
	if !reflect.DeepEqual(probeIDs, []string{"off-a"}) || disabledIDs != nil {
		t.Errorf("cross-provider partition = probe %v disabled %v", probeIDs, disabledIDs)
	}
}

// TestSplitDisabledModelIDs_PoolVirtualKey: a model disabled under a pool
// VIRTUAL key ("zhipu#<acct>") is out of rotation for the whole parent — the
// CLI partition expands the same keys TargetDisabled matches, so the refresh
// must not probe it (daemon pass parity).
func TestSplitDisabledModelIDs_PoolVirtualKey(t *testing.T) {
	dir := t.TempDir()
	clitest.SetPoolHome(t, dir)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"zhipu": {Provider: "zhipu"},
	}}
	vids, pooled := PoolVirtuals(cfg, "zhipu")
	if !pooled || len(vids) != 2 {
		t.Fatalf("pool setup: vids=%v pooled=%v", vids, pooled)
	}
	if err := runtimewire.SaveDisabledModelsFile(filepath.Join(dir, ".model-proxy", "disabled_models.json"),
		map[string][]string{vids[0]: {"off-a"}}); err != nil {
		t.Fatal(err)
	}
	probeIDs, disabledIDs := SplitDisabledModelIDs(cfg, "zhipu", []string{"on-a", "off-a"})
	if !reflect.DeepEqual(probeIDs, []string{"on-a"}) || !reflect.DeepEqual(disabledIDs, []string{"off-a"}) {
		t.Errorf("virtual-key disable not expanded: probe %v disabled %v", probeIDs, disabledIDs)
	}
}
