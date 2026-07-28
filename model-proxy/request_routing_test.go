package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testCatalog builds a models.dev catalog mapping model name → (context, input
// modalities) for request-routing tests. Names passed via tools are marked
// ToolCall=true (catalog tool_call metadata).
func testCatalog(entries map[string]struct {
	Context int64
	Input   []string
}, tools ...string) *modelsDevCatalog {
	byName := map[string]modelsDevModel{}
	for name, e := range entries {
		byName[name] = modelsDevModel{Context: e.Context, Input: e.Input}
	}
	for _, name := range tools {
		m := byName[name]
		m.ToolCall = true
		byName[name] = m
	}
	return &modelsDevCatalog{ByName: byName}
}

// TestModelFitsRequest: the unified predicate — a model fits iff it supports the
// request's image content (if any) and its known context window holds the
// estimated prompt. Conservative on unknowns: nil catalog → fits; unknown model
// with an image → does NOT fit (don't send images to an unknown model); unknown
// context + a big request → fits (don't block on an unknown limit).
func TestModelFitsRequest(t *testing.T) {
	cat := testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text":   {Context: 8000, Input: []string{"text"}},
		"vision": {Context: 8000, Input: []string{"text", "image"}},
		"big":    {Context: 128000, Input: []string{"text"}},
	}, "vision")
	img := []byte(`{"messages":[{"content":[{"type":"image"}]}]}`)
	text := []byte(`{"messages":[{"content":"hi"}]}`)
	big := []byte(`{"input":"` + strings.Repeat("qwxz!", 8000) + `"}`) // ~10k tokens
	tools := []byte(`{"messages":[{"content":"hi"}],"tools":[{"name":"f"}]}`)

	if !modelFitsRequest(cat, "text", text) {
		t.Error("text model + text request should fit")
	}
	if modelFitsRequest(cat, "text", img) {
		t.Error("text model + image request should NOT fit")
	}
	if modelFitsRequest(cat, "text", big) {
		t.Error("text model + big request should NOT fit (exceeds 8000)")
	}
	if !modelFitsRequest(cat, "vision", img) {
		t.Error("vision model + image request should fit")
	}
	if modelFitsRequest(cat, "vision", big) {
		t.Error("vision model + big request should NOT fit (exceeds 8000)")
	}
	if !modelFitsRequest(cat, "big", big) {
		t.Error("big model + big request should fit")
	}
	// Catalog-driven tools gate: a tools request only fits a model whose catalog
	// entry records tool_call support; unknown models are rejected (conservative).
	if !modelFitsRequest(cat, "vision", tools) {
		t.Error("tools-capable model + tools request should fit")
	}
	if modelFitsRequest(cat, "text", tools) {
		t.Error("tool-blind model + tools request should NOT fit")
	}
	if modelFitsRequest(cat, "unknown", tools) {
		t.Error("unknown model + tools request should NOT fit (can't confirm tool support)")
	}
	// Context boundary: est == window still fits (only est > window is rejected).
	// 10 + 31990 + 2 = 32002 non-CJK bytes → est = 32002/4 = 8000 exactly.
	exact := []byte(`{"input":"` + strings.Repeat("qwxz!", 6398) + `"}`)
	if !modelFitsRequest(cat, "text", exact) {
		t.Error("est == context window should fit (boundary is inclusive)")
	}
	// Conservative unknowns.
	if !modelFitsRequest(nil, "anything", img) {
		t.Error("nil catalog should fit (no metadata to constrain)")
	}
	if modelFitsRequest(cat, "unknown", img) {
		t.Error("unknown model + image should NOT fit (can't confirm image support)")
	}
	if !modelFitsRequest(cat, "unknown", big) {
		t.Error("unknown model + big request should fit (no known context limit)")
	}
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
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: "static"},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: "static"},
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
			"text-p":   {OpenAIBaseURL: textUp.URL, Provider: "static"},
			"vision-p": {OpenAIBaseURL: visionUp.URL, Provider: "static"},
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
			"text-p":   {OpenAIBaseURL: textUp.URL, Provider: "static"},
			"vision-p": {OpenAIBaseURL: visionUp.URL, Provider: "static"},
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

// TestModelFits_CapabilitiesOverride: a provider's `capabilities:` declaration is
// authoritative for the image/tools fit of the models it names — the catalog is
// ignored for them (the escape hatch for catalog blind spots). Undeclared models
// keep catalog behavior (conservative on unknowns).
func TestModelFits_CapabilitiesOverride(t *testing.T) {
	cat := testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text": {Context: 8000, Input: []string{"text"}},
	})
	caps := map[string][]string{
		"blind":       {"image"}, // not in catalog; declared image-only
		"blind-tools": {"tools"}, // not in catalog; declared tools-only
		"empty":       {},        // declared with NO capabilities
	}
	img := profileRequest([]byte(`{"messages":[{"content":[{"type":"image"}]}]}`))
	tools := profileRequest([]byte(`{"tools":[{"name":"x"}]}`))
	text := profileRequest([]byte(`{"messages":[{"content":"hi"}]}`))

	// ① Declared image → an image request fits even though the catalog doesn't
	// know the model at all.
	if !modelFits(cat, caps, "blind", img) {
		t.Error("declared [image] blind model + image request should fit")
	}
	// ② [image] declares image ONLY — a tools request does NOT fit (the
	// declaration is authoritative, not additive).
	if modelFits(cat, caps, "blind", tools) {
		t.Error("declared [image] model + tools request should NOT fit (tools not declared)")
	}
	if !modelFits(cat, caps, "blind-tools", tools) {
		t.Error("declared [tools] blind model + tools request should fit")
	}
	if modelFits(cat, caps, "blind-tools", img) {
		t.Error("declared [tools] model + image request should NOT fit (image not declared)")
	}
	if modelFits(cat, caps, "empty", img) || modelFits(cat, caps, "empty", tools) {
		t.Error("declared [] model should fit neither image nor tools requests")
	}
	if !modelFits(cat, caps, "empty", text) {
		t.Error("declared [] model + plain text request should fit (no capability demanded)")
	}
	// ③ Undeclared models keep catalog behavior.
	if modelFits(cat, caps, "text", img) {
		t.Error("undeclared text-only catalog model + image request should NOT fit")
	}
	if modelFits(cat, caps, "unknown", img) {
		t.Error("undeclared unknown model + image request should NOT fit (catalog conservative)")
	}
	if !modelFits(cat, caps, "unknown", text) {
		t.Error("undeclared unknown model + text request should fit")
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
			"text-p": {OpenAIBaseURL: textUp.URL, Provider: "static"},
			// Blind-spot provider: "gpt-blind" is NOT in the models.dev catalog;
			// without the capabilities declaration an image request would never
			// route to it.
			"blind-p": {OpenAIBaseURL: blindUp.URL, Provider: "static", Capabilities: map[string][]string{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

// TestTargetCapabilities_PooledVirtual: a credential-pool virtual id
// ("name#<accountID>") reads its PARENT provider's capabilities declaration.
func TestTargetCapabilities_PooledVirtual(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Capabilities: map[string][]string{"glm-x": {"image"}}},
	}}
	parentOf := map[string]string{"zhipu#abc123": "zhipu"}
	if caps := targetCapabilities(cfg, parentOf, RouteTarget{Provider: "zhipu#abc123", Model: "glm-x"}); !hasCapability(caps["glm-x"], "image") {
		t.Error("pooled virtual should resolve capabilities from its parent provider config")
	}
	if caps := targetCapabilities(cfg, parentOf, RouteTarget{Provider: "zhipu", Model: "glm-x"}); !hasCapability(caps["glm-x"], "image") {
		t.Error("non-virtual name should resolve capabilities directly")
	}
	if caps := targetCapabilities(cfg, parentOf, RouteTarget{Provider: "unknown", Model: "m"}); caps != nil {
		t.Error("unknown provider should yield nil capabilities")
	}
}

// TestCollectCrossRoute_DedupIgnoresPriority: the same {provider, model, protocol}
// appearing in multiple routes at DIFFERENT priorities is collected ONCE (the best
// priority), not twice. Previously the dedup key was the whole RouteTarget value
// (which includes Priority), so the duplicate slipped through and could be called
// twice during a cross-route fallback. Pooled virtuals (distinct provider ids) are
// NOT collapsed.
func TestCollectCrossRoute_DedupIgnoresPriority(t *testing.T) {
	expanded := map[string][]RouteTarget{
		"routeA": {{Provider: "zhipu", Model: "glm", Protocol: "openai", Priority: 1}},
		"routeB": {{Provider: "zhipu", Model: "glm", Protocol: "openai", Priority: 3}}, // same backend, diff priority
		"routeC": {{Provider: "deepseek", Model: "ds", Protocol: "openai", Priority: 2}},
		"routeD": {{Provider: "zhipu#acct-b", Model: "glm", Protocol: "openai", Priority: 1}}, // a sibling pool virtual — distinct
	}
	pool := collectCrossRoute(expanded, func(RouteTarget) bool { return true })
	if len(pool) != 3 {
		t.Fatalf("pool has %d targets, want 3 (zhipu/glm/openai deduped to one, deepseek/ds, zhipu#acct-b): %+v", len(pool), pool)
	}
	// The deduped zhipu/glm/openai keeps the BEST priority (1), not 3.
	var zhipu *RouteTarget
	for i := range pool {
		if pool[i].Provider == "zhipu" && pool[i].Model == "glm" {
			zhipu = &pool[i]
		}
	}
	if zhipu == nil {
		t.Fatal("zhipu/glm/openai missing from pool")
	}
	if zhipu.Priority != 1 {
		t.Errorf("deduped zhipu priority = %d, want 1 (best priority kept)", zhipu.Priority)
	}
}
