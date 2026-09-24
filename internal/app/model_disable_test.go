// model_disable_test.go — behavior contract for the operator disabled-model
// override (Web Status→Models Disable/Enable, POST /api/models/disable):
// scheduling drops disabled (provider, model) targets, a fully disabled
// exposed name disappears from GET /v1/models and answers 404 (distinct from
// the unknown-model 502), a partially disabled route fails over to its
// remaining targets, and the /debug/route preview mirrors the live path.
package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	runtimestate "model-proxy/internal/runtime"
)

func modelDisableTestConfig(primary, fallback *httptest.Server) *configdomain.Config {
	return &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
			"solo": {
				{Provider: "primary", Model: "solo", Priority: 1},
			},
		},
		Scheduling: configdomain.Scheduling{CircuitThreshold: 3},
	}
}

// TestModelDisable_PartialDisableFailsOver pins the "no longer effective
// internally" half: disabling one provider's target removes it from routing
// while the exposed name stays listed and servable through the sibling.
func TestModelDisable_PartialDisableFailsOver(t *testing.T) {
	var pHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pHits.Add(1)
		w.Write([]byte(`{"ok":"primary"}`))
	}))
	defer primary.Close()
	fallback, fSeen := newCaptureUpstream(200, `{"ok":"fallback"}`)
	defer fallback.Close()

	p := newTestProxy(t, modelDisableTestConfig(primary, fallback))
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	post := func(model string) *http.Response {
		t.Helper()
		resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
			stringReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("client post: %v", err)
		}
		resp.Body.Close()
		return resp
	}

	// Baseline: priority routes to primary.
	if resp := post("m1"); resp.StatusCode != 200 {
		t.Fatalf("baseline m1 status = %d, want 200", resp.StatusCode)
	}
	if pHits.Load() != 1 {
		t.Fatalf("baseline primary hits = %d, want 1", pHits.Load())
	}

	// Disable (primary, m1): requests fail over to the sibling, the exposed
	// name stays listed, and the sibling keeps answering after re-requests.
	p.runtimeState.SetModelDisabled("primary", "m1", true)
	if resp := post("m1"); resp.StatusCode != 200 {
		t.Fatalf("disabled-primary m1 status = %d, want 200 via fallback", resp.StatusCode)
	}
	if pHits.Load() != 1 {
		t.Fatalf("primary hits after disable = %d, want 1 (disabled target must not be scheduled)", pHits.Load())
	}
	if len(*fSeen) != 1 {
		t.Fatalf("fallback requests = %v, want exactly one failover attempt", *fSeen)
	}
	if ids := exposedModelIDs(t, px); !contains(ids, "m1") {
		t.Fatalf("GET /v1/models = %v after partial disable, want m1 still listed", ids)
	}

	// Re-enable: primary is schedulable again on the next decision. (The
	// end-to-end request would still park on fallback — route sticky dwell —
	// which is correct scheduling, not a disable leak.)
	p.runtimeState.SetModelDisabled("primary", "m1", false)
	ordered, _ := p.decideOrder(p.cfg, p.parentOf, "m1", "", p.expandedRoutes["m1"], time.Now(), false, p.routeKeys)
	if len(ordered) != 2 || (ordered[0].Provider != "primary" && ordered[1].Provider != "primary") {
		t.Fatalf("re-enabled order = %v, want both targets back (primary schedulable again)", ordered)
	}
}

// TestModelDisable_FullyDisabledModelHidesAndAnswers404 pins both halves for
// a model whose every target is disabled: hidden from GET /v1/models, and a
// direct request answers 404 "is disabled" — distinct from the unknown-model
// 502 "not found in routes". The /debug/route preview mirrors the same
// verdict without an upstream call.
func TestModelDisable_FullyDisabledModelHidesAndAnswers404(t *testing.T) {
	var pHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pHits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer primary.Close()
	fallback, _ := newCaptureUpstream(200, `{}`)
	defer fallback.Close()

	p := newTestProxy(t, modelDisableTestConfig(primary, fallback))
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	if ids := exposedModelIDs(t, px); !contains(ids, "solo") {
		t.Fatalf("GET /v1/models = %v before disable, want solo listed", ids)
	}

	p.runtimeState.SetModelDisabled("primary", "solo", true)

	if ids := exposedModelIDs(t, px); contains(ids, "solo") {
		t.Fatalf("GET /v1/models = %v after disable, want solo hidden", ids)
	}
	if ids := exposedModelIDs(t, px); !contains(ids, "m1") {
		t.Fatalf("GET /v1/models = %v after disable, want unrelated m1 still listed", ids)
	}

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
		stringReader(`{"model":"solo","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("client post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled model status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(string(body), "is disabled") {
		t.Fatalf("disabled model body = %q, want it to name the disable", body)
	}
	if pHits.Load() != 0 {
		t.Fatalf("upstream hits = %d, want 0 (disabled model must not dial upstream)", pHits.Load())
	}

	// An unknown model keeps its distinct 502 not-found terminal.
	resp2, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
		stringReader(`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("client post: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("unknown model status = %d, want 502", resp2.StatusCode)
	}

	// /debug/route previews the disabled verdict without an upstream call.
	prevResp, err := http.Post(px.URL+"/debug/route", "application/json",
		stringReader(`{"model":"solo","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("route preview post: %v", err)
	}
	var preview map[string]any
	if err := json.NewDecoder(prevResp.Body).Decode(&preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	prevResp.Body.Close()
	if preview["route_found"] != false {
		t.Fatalf("preview route_found = %v, want false", preview["route_found"])
	}
	if msg, _ := preview["error"].(string); !strings.Contains(msg, "is disabled") {
		t.Fatalf("preview error = %v, want the disable reason", preview["error"])
	}
}

// TestModelDisable_PreviewOrderAndPinInterplay covers the Manager-side
// contract: decideOrder drops disabled candidates BEFORE pin narrowing (a pin
// onto a disabled target falls back to the remaining candidates), and the
// detached Dashboard preview shows the same chain the live path schedules.
func TestModelDisable_PreviewOrderAndPinInterplay(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer primary.Close()
	fallback, _ := newCaptureUpstream(200, `{}`)
	defer fallback.Close()

	p := newTestProxy(t, modelDisableTestConfig(primary, fallback))
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}

	// Pin m1 to primary, then disable (primary, m1): the pin falls back —
	// scheduling returns the fallback target instead of the disabled one.
	p.runtimeState.SetPin("m1", runtimestate.Pin{Provider: "primary"})
	p.runtimeState.SetModelDisabled("primary", "m1", true)

	now := time.Now()
	ordered, _ := p.decideOrder(p.cfg, p.parentOf, "m1", "", p.expandedRoutes["m1"], now, false, p.routeKeys)
	if len(ordered) != 1 || ordered[0].Provider != "fallback" {
		t.Fatalf("pinned-but-disabled order = %v, want [fallback]", ordered)
	}

	// The detached dashboard preview agrees with the live decision.
	dash := p.runtimeState.Dashboard(now)
	runtimeTargets := make([]runtimestate.Target, 0, len(p.expandedRoutes["m1"]))
	for _, t := range p.expandedRoutes["m1"] {
		runtimeTargets = append(runtimeTargets, runtimestate.Target{
			Provider: t.Provider,
			Parent:   p.parentOf[t.Provider],
			Model:    t.Model,
			Priority: t.Priority,
		})
	}
	routeKeys := map[string]bool{}
	for k := range p.expandedRoutes {
		routeKeys[k] = true
	}
	preview := dash.PreviewOrder(runtimestate.ScheduleInput{
		Exposed:   "m1",
		Targets:   runtimeTargets,
		RouteKeys: routeKeys,
		Dwell:     time.Minute,
		Now:       now,
	})
	if len(preview.Order) != 1 {
		t.Fatalf("preview order = %v, want exactly the fallback target", preview.Order)
	}

	// DisabledModels projects the override as a detached sorted map.
	disabled := p.runtimeState.DisabledModels()
	if len(disabled) != 1 || len(disabled["primary"]) != 1 || disabled["primary"][0] != "m1" {
		t.Fatalf("DisabledModels() = %v, want {primary:[m1]}", disabled)
	}
}

func exposedModelIDs(t *testing.T, px *httptest.Server) []string {
	t.Helper()
	resp, err := http.Get(px.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestModelDisable_APIModelsProjection pins the admin read path: GET
// /api/models carries the override as the sorted disabled map, and POST
// /api/models/disable round-trips through validation into the same Manager
// state the forward path consults (unknown pairs are refused with 400).
func TestModelDisable_APIModelsProjection(t *testing.T) {
	w, p := newTestWeb(t)
	// The template config's zhipu provider serves no models by default;
	// route one explicitly so the pair validates.
	p.mu.Lock()
	p.cfg.Providers["zhipu"] = configdomain.Provider{Provider: "zhipu", OpenAIBaseURL: "https://x", Models: []string{"glm-4.7"}}
	p.mu.Unlock()

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest(http.MethodPost, "/api/models/disable",
		strings.NewReader(`{"provider":"zhipu","model":"glm-4.7","disabled":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/models/disable = %d: %s", rec.Code, rec.Body.String())
	}
	if !p.runtimeState.ModelDisabled("zhipu", "glm-4.7") {
		t.Fatal("toggle did not reach the runtime Manager state")
	}

	rec = httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models = %d", rec.Code)
	}
	var doc struct {
		Disabled map[string][]string `json:"disabled"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode /api/models: %v", err)
	}
	if !reflect.DeepEqual(doc.Disabled, map[string][]string{"zhipu": {"glm-4.7"}}) {
		t.Fatalf("disabled projection = %v, want {zhipu:[glm-4.7]}", doc.Disabled)
	}

	// Fail-closed: an unserved pair is a 400 and installs nothing.
	rec = httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest(http.MethodPost, "/api/models/disable",
		strings.NewReader(`{"provider":"zhipu","model":"nope","disabled":true}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown model toggle = %d, want 400", rec.Code)
	}
	if p.runtimeState.ModelDisabled("zhipu", "nope") {
		t.Fatal("invalid pair installed a disable override")
	}
}

// TestModelDisable_ScheduleStatusOmitsFullyDisabledRoutes pins the Status→
// Schedule projection: a route whose every target is operator-disabled is
// omitted from schedule.models entirely (it is hidden from /v1/models and
// unservable — an empty chain block is noise); a partially disabled route
// keeps listing with its remaining chain.
func TestModelDisable_ScheduleStatusOmitsFullyDisabledRoutes(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer primary.Close()
	fallback, _ := newCaptureUpstream(200, `{}`)
	defer fallback.Close()

	p := newTestProxy(t, modelDisableTestConfig(primary, fallback))
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}

	// Fully disabled (solo → primary only): omitted.
	p.runtimeState.SetModelDisabled("primary", "solo", true)
	// Partially disabled (m1 → primary disabled, fallback remains): listed
	// with the fallback-only chain.
	p.runtimeState.SetModelDisabled("primary", "m1", true)

	var st struct {
		Models map[string]struct {
			Ordered []struct {
				Provider string `json:"provider"`
			} `json:"ordered"`
		} `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatalf("parse scheduleStatus: %v", err)
	}
	if _, ok := st.Models["solo"]; ok {
		t.Fatal("fully disabled route solo still listed in schedule.models")
	}
	ri, ok := st.Models["m1"]
	if !ok {
		t.Fatal("partially disabled route m1 missing from schedule.models")
	}
	if len(ri.Ordered) != 1 || ri.Ordered[0].Provider != "fallback" {
		t.Fatalf("m1 ordered = %+v, want the non-disabled fallback only", ri.Ordered)
	}
}
