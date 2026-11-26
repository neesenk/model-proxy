package forward

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"model-proxy/internal/catalog"
	"model-proxy/internal/observe/requestlog"
)

// r6ImageRequest calls route "m" with an image. Neither m target accepts
// images in r6RerouteCatalog, so request-aware routing drops both and has to
// look cross-route — the reroute that x-mp-reroute and schedule.note trace.
const r6ImageRequest = `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"hi"}]}]}`

// r6DroppedMTargets is the fit trace of route "m" for the image request.
const r6DroppedMTargets = "request-aware: dropped a/ma: model metadata lacks image modality; b/mb: model metadata lacks image modality"

func r6RerouteCatalog() *catalog.Catalog {
	sighted := catalog.Modalities{Input: []string{"text", "image"}}
	return catalog.New(map[string]catalog.Model{
		"ma": {Context: 200000},
		"mb": {Context: 200000},
		"mx": {Context: 200000, Modalities: sighted},
		"my": {Context: 200000, Modalities: sighted},
	})
}

func r6Only(pool []RouteTarget, provider string) []RouteTarget {
	for _, target := range pool {
		if target.Provider == provider {
			return []RouteTarget{target}
		}
	}
	return nil
}

// r6RerouteServe builds the image-request scenario and serves it once: route
// "m" targets a/ma and b/mb, route "n" the image-capable donors x/mx and
// y/my, every provider fronted by its own fake upstream (respond names the
// responder per provider). schedulePool decides what the cross-route pool
// yields per wait-retry round (1-based) — nil models a pool that cooled down
// entirely, which leaves the route's own targets in place. source is the
// scheduling-engine name the route's own scheduling reports ("" = none, the
// pass-through default). The cooldown seam is programmed so every failed
// round is followed by another round, up to the loop's bound.
func r6RerouteServe(
	t *testing.T,
	h *harness,
	respond func(provider string) http.HandlerFunc,
	schedulePool func(round int, pool []RouteTarget) []RouteTarget,
	source string,
) (*httptest.ResponseRecorder, map[string]*fakeUpstream) {
	t.Helper()
	upstreams := map[string]*fakeUpstream{}
	providers := map[string]Provider{}
	for _, name := range []string{"a", "b", "x", "y"} {
		upstreams[name] = newFakeUpstream(t, respond(name))
		providers[name] = Provider{AnthropicBaseURL: upstreams[name].srv.URL, Provider: "test-static"}
	}
	rounds := 0
	h.svc.Schedule = func(_ *Config, _ map[string]string, exposed, _ string, _ bool, targets []RouteTarget, _ map[string]bool, _ uint64) ([]RouteTarget, string) {
		if exposed != "m#req" {
			return targets, source
		}
		rounds++
		return schedulePool(rounds, targets), ""
	}
	h.state.allDown = true
	h.state.allRateLimited = true
	// earliestIn anchors the recovery window at each DECISION (harness seam):
	// the wait-retry branch holds by construction however long a round takes.
	h.state.earliestIn = 25 * time.Millisecond
	snap := h.snapshot(&Config{
		Providers:  providers,
		Routes:     map[string][]RouteTarget{"m": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}}, "n": {{Provider: "x", Model: "mx"}, {Provider: "y", Model: "my"}}},
		Scheduling: Scheduling{RetryWait: "2s"},
	})
	snap.Catalog = r6RerouteCatalog()
	return h.serve(snap, "anthropic", "/v1/messages", r6ImageRequest, nil), upstreams
}

func r6RateLimited(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusTooManyRequests)
}

// TestR6ServeRerouteHeaderFollowsTheLatestRound: the wait-retry rounds of one
// request share one ResponseWriter, and each round decides the reroute
// afresh (the cross-route pool is scheduled again, so a donor that cooled
// down drops out). x-mp-reroute must describe the round that actually served
// the request: an earlier round's trace may not outlive it.
func r6AnthropicOKResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"` + text + `"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":5}}`))
	}
}

func TestR6ServeRerouteHeaderFollowsTheLatestRound(t *testing.T) {
	answer := func(provider string) http.HandlerFunc { return r6AnthropicOKResponder("answered-by-" + provider) }
	xThenCooled := func(round int, pool []RouteTarget) []RouteTarget {
		if round == 1 {
			return r6Only(pool, "x")
		}
		return nil
	}

	t.Run("single rerouted round carries its trace", func(t *testing.T) {
		h := newHarness()
		w, _ := r6RerouteServe(t, h, answer, func(_ int, pool []RouteTarget) []RouteTarget { return r6Only(pool, "x") }, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want the cross-route commit", w.Code, w.Body.String())
		}
		if got, want := w.Header().Get("x-mp-reroute"), r6DroppedMTargets+" → cross-route x/mx"; got != want {
			t.Fatalf("x-mp-reroute = %q, want %q", got, want)
		}
	})

	t.Run("later round without a reroute clears the earlier trace", func(t *testing.T) {
		h := newHarness()
		w, upstreams := r6RerouteServe(t, h, func(provider string) http.HandlerFunc {
			if provider == "x" {
				return r6RateLimited
			}
			return answer(provider)
		}, xThenCooled, "")
		if w.Code != http.StatusOK || w.Body.String() == "" {
			t.Fatalf("status = %d (%s), want the second round's commit by the route's own target", w.Code, w.Body.String())
		}
		// Round 1 rerouted to x/mx and was rate-limited; round 2 found the
		// pool cooled down and served with a/ma.
		if upstreams["x"].hits() != 1 || upstreams["a"].hits() != 1 {
			t.Fatalf("hits x=%d a=%d, want 1/1 (one rate-limited rerouted round, then the route's own target)", upstreams["x"].hits(), upstreams["a"].hits())
		}
		if got := w.Header().Get("x-mp-reroute"); got != "" {
			t.Fatalf("x-mp-reroute = %q, want none: the serving round did not reroute, so the earlier round's trace is stale", got)
		}
	})

	t.Run("later round rerouting elsewhere carries its own trace", func(t *testing.T) {
		h := newHarness()
		w, upstreams := r6RerouteServe(t, h, func(provider string) http.HandlerFunc {
			if provider == "x" {
				return r6RateLimited
			}
			return answer(provider)
		}, func(round int, pool []RouteTarget) []RouteTarget {
			if round == 1 {
				return r6Only(pool, "x")
			}
			return r6Only(pool, "y")
		}, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want the second round's cross-route commit", w.Code, w.Body.String())
		}
		if upstreams["y"].hits() != 1 {
			t.Fatalf("y hits = %d, want 1 (the second round's donor)", upstreams["y"].hits())
		}
		if got, want := w.Header().Get("x-mp-reroute"), r6DroppedMTargets+" → cross-route y/my"; got != want {
			t.Fatalf("x-mp-reroute = %q, want %q", got, want)
		}
	})
}

// TestR6ServeTerminalRowRerouteNoteFollowsTheLatestRound: schedule.note is the
// request log's projection of the same per-round fact as x-mp-reroute, and
// the all-targets-failed terminal row is written after the LAST round — it
// must not carry an earlier round's trace either. The route's scheduling
// names an engine, so the terminal row's schedule is always present here:
// dropping the stale note must drop exactly the note, never the schedule.
func TestR6ServeTerminalRowRerouteNoteFollowsTheLatestRound(t *testing.T) {
	const source = "load_balance"
	rateLimited := func(string) http.HandlerFunc { return r6RateLimited }

	t.Run("final round rerouted keeps the note", func(t *testing.T) {
		h, rows := newTerminalLogHarness(t)
		w, upstreams := r6RerouteServe(t, h, rateLimited, func(_ int, pool []RouteTarget) []RouteTarget { return r6Only(pool, "x") }, source)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want the 429 terminal", w.Code)
		}
		if upstreams["x"].hits() != 3 {
			t.Fatalf("x hits = %d, want 3 (every round rerouted to the rate-limited donor)", upstreams["x"].hits())
		}
		_, terminal := splitTerminalRows(t, rows())
		if terminal == nil {
			t.Fatal("no terminal row")
		}
		if terminal.Schedule == nil || *terminal.Schedule != (requestlog.ScheduleDecision{Source: source, Note: r6DroppedMTargets + " → cross-route x/mx"}) {
			t.Errorf("terminal schedule = %+v, want the engine and the final round's reroute note", terminal.Schedule)
		}
	})

	t.Run("later round without a reroute drops the earlier note", func(t *testing.T) {
		h, rows := newTerminalLogHarness(t)
		w, upstreams := r6RerouteServe(t, h, rateLimited, func(round int, pool []RouteTarget) []RouteTarget {
			if round == 1 {
				return r6Only(pool, "x")
			}
			return nil
		}, source)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want the 429 terminal", w.Code)
		}
		// Round 1 rerouted to x/mx; rounds 2 and 3 ran the route's own targets.
		if upstreams["x"].hits() != 1 || upstreams["a"].hits() != 2 || upstreams["b"].hits() != 2 {
			t.Fatalf("hits x=%d a=%d b=%d, want 1/2/2", upstreams["x"].hits(), upstreams["a"].hits(), upstreams["b"].hits())
		}
		_, terminal := splitTerminalRows(t, rows())
		if terminal == nil {
			t.Fatal("no terminal row")
		}
		// A missing schedule is no answer either: the row must still carry the
		// pass's schedule, just without a reroute note.
		if terminal.Schedule == nil || terminal.Schedule.Note != "" {
			t.Fatalf("terminal schedule = %+v, want the final round's schedule without a reroute note", terminal.Schedule)
		}
		if terminal.Schedule.Source != source {
			t.Errorf("terminal schedule source = %q, want %q (only the stale note is dropped)", terminal.Schedule.Source, source)
		}
	})
}
