package models

import (
	"context"
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/probe"
	"model-proxy/internal/protocol"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY")

	// Per (path, model) status table. The 3-leg probe hits /chat/completions and
	// /responses on the openai base (the anthropic leg is unprobed - no
	// anthropic_base_url). A model is KEPT when ANY leg classifies Yes (2xx
	// here); 404 -> No, 500 -> Unknown, both drop the model. Input order must be
	// preserved in the outputs.
	type legKey struct{ path, model string }
	statuses := map[legKey]int{
		{"/chat/completions", "keep-a"}:      200,
		{"/responses", "keep-a"}:             200,
		{"/chat/completions", "drop-b"}:      404,
		{"/responses", "drop-b"}:             404,
		{"/chat/completions", "resp-only-c"}: 404,
		{"/responses", "resp-only-c"}:        200,
		{"/chat/completions", "drop-d"}:      500,
		{"/responses", "drop-d"}:             500,
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
	ids := []string{"keep-a", "drop-b", "resp-only-c", "drop-d", "keep-e"}
	kept, dropped, protocols, err := CheckProviderModels(cfg, "zhipu", ids)
	if err != nil {
		t.Fatalf("checkProviderModels: %v", err)
	}
	wantKept := []string{"keep-a", "resp-only-c", "keep-e"}
	if len(kept) != len(wantKept) {
		t.Fatalf("kept=%v want %v (a model is kept when ANY leg is callable)", kept, wantKept)
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
	if dropped[1].Status != 500 {
		t.Errorf("drop-d status=%d want 500", dropped[1].Status)
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

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"},
		},
	}
	kept, dropped, _, err := CheckProviderModels(cfg, "zhipu", []string{"glm-5.2"})
	if err != nil {
		t.Fatalf("not-logged-in: want no error (impl builds file-backed), got %v", err)
	}
	if len(kept) != 0 {
		t.Errorf("not-logged-in: kept=%v want empty (auth fails on every leg)", kept)
	}
	if len(dropped) != 1 || dropped[0].Model != "glm-5.2" {
		t.Errorf("not-logged-in: dropped=%+v want [glm-5.2]", dropped)
	}
	// Every probed leg failed at the auth step (no HTTP exchange -> status 0);
	// the per-leg summary carries each leg's auth error, and the anthropic leg
	// is reported as unprobed (no anthropic_base_url configured).
	if dropped[0].Status != 0 || !strings.Contains(dropped[0].Reason, "auth") {
		t.Errorf("not-logged-in: dropped reason=%q status=%d want auth error / status 0", dropped[0].Reason, dropped[0].Status)
	}
	if !strings.Contains(dropped[0].Reason, "anthropic not probed (no base)") {
		t.Errorf("not-logged-in: dropped reason=%q want the anthropic leg marked unprobed", dropped[0].Reason)
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
	out := grabStdout(t, func() {
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
	out := grabStdout(t, func() { PrintKeptModels("x", nil, nil, nil, nil, nil) })
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
	setPoolHome(t, dir)

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
	setPoolHome(t, t.TempDir())
	if got := loadModelCapsProjection(cfg); got != nil {
		t.Errorf("missing file: projection=%v want nil", got)
	}

	// Malformed file -> nil, no error surfaced.
	dir2 := t.TempDir()
	setPoolHome(t, dir2)
	bad := filepath.Join(dir2, ".model-proxy", "model_caps.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadModelCapsProjection(cfg); got != nil {
		t.Errorf("malformed file: projection=%v want nil", got)
	}
}
