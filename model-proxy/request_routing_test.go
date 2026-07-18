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
// modalities) for request-routing tests.
func testCatalog(entries map[string]struct {
	Context int64
	Input   []string
}) *modelsDevCatalog {
	byName := map[string]modelsDevModel{}
	for name, e := range entries {
		byName[name] = modelsDevModel{Context: e.Context, Input: e.Input}
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
	})
	img := []byte(`{"messages":[{"content":[{"type":"image"}]}]}`)
	text := []byte(`{"messages":[{"content":"hi"}]}`)
	big := []byte(`{"input":"` + strings.Repeat("x", 40000) + `"}`) // ~10k tokens

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
	p := NewProxy(cfg)
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
	body := `{"model":"glm","input":"` + strings.Repeat("x", 40000) + `"}`
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
	p := NewProxy(cfg)
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
	p := NewProxy(cfg)
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
