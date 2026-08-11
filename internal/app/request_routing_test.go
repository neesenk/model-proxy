package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/catalog"
)

// testCatalog builds a models.dev catalog mapping model name → (context, input
// modalities) for request-routing tests. Names passed via tools are marked
// ToolCall=true (catalog tool_call metadata).
func testCatalog(entries map[string]struct {
	Context int64
	Input   []string
}, tools ...string) *catalog.Catalog {
	byName := map[string]catalog.Model{}
	for name, e := range entries {
		byName[name] = catalog.Model{Context: e.Context, Modalities: catalog.Modalities{Input: e.Input}}
	}
	for _, name := range tools {
		m := byName[name]
		m.ToolCall = true
		byName[name] = m
	}
	return catalog.New(byName)
}

// TestForward_ContextCrossRoute: a big request to a small-context route falls
// back CROSS-ROUTE to a big-context model in another route, selected by the
// normal scheduling policy (automatic — no config key needed).
func TestForward_ContextCrossRoute(t *testing.T) {
	var smallHit, bigHit bool
	smallUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		smallHit = true
		w.Write([]byte(`{}`))
	}))
	defer smallUp.Close()
	bigUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bigHit = true
		w.Write([]byte(`{}`))
	}))
	defer bigUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm":      {{Provider: "small-prov", Model: "small"}},
			"glm-long": {{Provider: "big-prov", Model: "big"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.providers["big-prov"] = &testProv{key: "b"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{"small": {Context: 8000}, "big": {Context: 128000}})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// ~10k estimated tokens > small's 8000 → in-route (small) doesn't fit →
	// cross-route pool picks big → big-prov is hit, small-prov is not.
	body := `{"model":"glm","input":"` + strings.Repeat("qwxz!", 8000) + `"}`
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if !bigHit {
		t.Error("cross-route fallback did not hit big-prov (the only model that fits)")
	}
	if smallHit {
		t.Error("small-prov was hit — should be skipped (context too small for the request)")
	}
}

// TestForward_CapabilityCrossRoute: an image request to a text-only route falls
// back cross-route to a vision-capable model in another route. A text request to
// the same route stays in-route (everything fits).
func TestForward_CapabilityCrossRoute(t *testing.T) {
	var textHit, visionHit bool
	textUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		textHit = true
		w.Write([]byte(`{}`))
	}))
	defer textUp.Close()
	visionUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionHit = true
		w.Write([]byte(`{}`))
	}))
	defer visionUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"text-p":   {OpenAIBaseURL: textUp.URL, Provider: testProviderID},
			"vision-p": {OpenAIBaseURL: visionUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm":        {{Provider: "text-p", Model: "text"}},
			"glm-vision": {{Provider: "vision-p", Model: "vision"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["text-p"] = &testProv{key: "t"}
	p.providers["vision-p"] = &testProv{key: "v"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text":   {Context: 8000, Input: []string{"text"}},
		"vision": {Context: 8000, Input: []string{"text", "image"}},
	})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	post := func(body string) {
		textHit, visionHit = false, false
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Image request to "glm" (text-only route) → text doesn't fit → cross-route
	// pool picks vision → vision-p hit.
	post(`{"model":"glm","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`)
	if !visionHit || textHit {
		t.Errorf("image request: text=%v vision=%v, want cross-route to vision only", textHit, visionHit)
	}
	// Text request to "glm" → text fits → stays in-route.
	post(`{"model":"glm","messages":[{"role":"user","content":"hi"}]}`)
	if !textHit || visionHit {
		t.Errorf("text request: text=%v vision=%v, want in-route text only", textHit, visionHit)
	}
}

// TestForward_CapabilityFilter_E2E: an image request is routed to the
// image-capable target; a text request uses the normal (priority-first) target.
func TestForward_CapabilityFilter_E2E(t *testing.T) {
	var textHit, visionHit bool
	textUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		textHit = true
		w.Write([]byte(`{}`))
	}))
	defer textUp.Close()
	visionUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionHit = true
		w.Write([]byte(`{}`))
	}))
	defer visionUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"text-p":   {OpenAIBaseURL: textUp.URL, Provider: testProviderID},
			"vision-p": {OpenAIBaseURL: visionUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {
				{Provider: "text-p", Model: "text", Priority: 1},
				{Provider: "vision-p", Model: "vision", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["text-p"] = &testProv{key: "t"}
	p.providers["vision-p"] = &testProv{key: "v"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text":   {Context: 8000, Input: []string{"text"}},
		"vision": {Context: 8000, Input: []string{"text", "image"}},
	})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	post := func(body string) {
		textHit, visionHit = false, false
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Image request → vision-p (the only image-capable target).
	post(`{"model":"glm","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`)
	if !visionHit || textHit {
		t.Errorf("image request: text=%v vision=%v, want vision only", textHit, visionHit)
	}
	// Text request → text-p (priority 1, no filtering).
	post(`{"model":"glm","messages":[{"role":"user","content":"hi"}]}`)
	if !textHit || visionHit {
		t.Errorf("text request: text=%v vision=%v, want text only", textHit, visionHit)
	}
}

// TestForward_CapabilitiesOverride_E2E: a model the catalog does NOT know (blind
// spot) becomes routable for image requests once its provider declares
// capabilities — an image request is routed to it; a text request still prefers
// the priority-1 target.
func TestForward_CapabilitiesOverride_E2E(t *testing.T) {
	var textHit, blindHit bool
	textUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		textHit = true
		w.Write([]byte(`{}`))
	}))
	defer textUp.Close()
	blindUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blindHit = true
		w.Write([]byte(`{}`))
	}))
	defer blindUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"text-p": {OpenAIBaseURL: textUp.URL, Provider: testProviderID},
			// Blind-spot provider: "gpt-blind" is NOT in the models.dev catalog;
			// without the capabilities declaration an image request would never
			// route to it.
			"blind-p": {OpenAIBaseURL: blindUp.URL, Provider: testProviderID, Capabilities: map[string][]string{
				"gpt-blind": {"image"},
			}},
		},
		Routes: map[string][]RouteTarget{
			"glm": {
				{Provider: "text-p", Model: "text", Priority: 1},
				{Provider: "blind-p", Model: "gpt-blind", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["text-p"] = &testProv{key: "t"}
	p.providers["blind-p"] = &testProv{key: "b"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text": {Context: 8000, Input: []string{"text"}},
	})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	post := func(body string) {
		textHit, blindHit = false, false
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// Image request → blind-p (declared image-capable; the only fitting target).
	post(`{"model":"glm","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`)
	if !blindHit || textHit {
		t.Errorf("image request: text=%v blind=%v, want blind only (capabilities override)", textHit, blindHit)
	}
	// Text request → text-p (priority 1, no capability filtering).
	post(`{"model":"glm","messages":[{"role":"user","content":"hi"}]}`)
	if !textHit || blindHit {
		t.Errorf("text request: text=%v blind=%v, want text only", textHit, blindHit)
	}
}

// TestForward_ForceProviderIncompatibleTargetDoesNotCrossRoute proves that a
// request-scoped replay override remains exclusive even when request-aware
// routing considers the forced model capability-incompatible.
func TestForward_ForceProviderIncompatibleTargetDoesNotCrossRoute(t *testing.T) {
	type upstreamObservation struct {
		model string
		err   error
	}
	forcedObservation := make(chan upstreamObservation, 1)
	forcedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		err := json.NewDecoder(request.Body).Decode(&body)
		forcedObservation <- upstreamObservation{model: body.Model, err: err}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"forced":true}`))
	}))
	defer forcedUpstream.Close()

	var compatibleHits atomic.Int64
	compatibleUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		compatibleHits.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"compatible":true}`))
	}))
	defer compatibleUpstream.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"forced-text": {
				OpenAIBaseURL: forcedUpstream.URL,
				Provider:      testProviderID,
			},
			"compatible-vision": {
				OpenAIBaseURL: compatibleUpstream.URL,
				Provider:      testProviderID,
			},
		},
		Routes: map[string][]RouteTarget{
			"public": {
				{Provider: "forced-text", Model: "text-model"},
			},
			"vision-route": {
				{Provider: "compatible-vision", Model: "vision-model"},
			},
		},
	}
	proxy := newTestProxy(t, cfg)
	proxy.providers["forced-text"] = &testProv{key: "forced"}
	proxy.providers["compatible-vision"] = &testProv{key: "vision"}
	proxy.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text-model":   {Context: 8000, Input: []string{"text"}},
		"vision-model": {Context: 8000, Input: []string{"text", "image"}},
	})
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	request, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v1/responses",
		strings.NewReader(`{"model":"public","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-mp-force-provider", "forced-text")
	response, err := http.DefaultClient.Do(request.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close response: %v", closeErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%q, want 200", response.StatusCode, responseBody)
	}
	if string(responseBody) != `{"forced":true}` {
		t.Fatalf("body = %q, want forced upstream response", responseBody)
	}

	var observation upstreamObservation
	select {
	case observation = <-forcedObservation:
	case <-time.After(2 * time.Second):
		t.Fatal("forced upstream observation was not delivered")
	}
	if observation.err != nil {
		t.Fatalf("forced upstream decode: %v", observation.err)
	}
	if observation.model != "text-model" {
		t.Errorf("forced upstream model = %q, want text-model", observation.model)
	}
	if hits := compatibleHits.Load(); hits != 0 {
		t.Errorf("compatible cross-route upstream hits = %d, want 0", hits)
	}
}
