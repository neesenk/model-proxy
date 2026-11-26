package mcp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// r6_route_tools_refresh_test.go pins the route tools/list cache policy
// (R6 B21): a complete aggregation is cached for the session's lifetime, but
// a DEGRADED one (some backend failed) only until a short refresh point —
// past it the next access re-aggregates instead of serving a backend's
// transient failure for the remaining half hour.

// r6Backends is a fake backend set behind a route: per-server fetch counts
// and the servers whose fetch fails.
type r6Backends struct {
	mu      sync.Mutex
	fetches map[string]int
	down    map[string]bool
}

func newR6Backends(down ...string) *r6Backends {
	b := &r6Backends{fetches: map[string]int{}, down: map[string]bool{}}
	for _, server := range down {
		b.down[server] = true
	}
	return b
}

func (b *r6Backends) fetch(server string) ([]ToolSpec, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fetches[server]++
	if b.down[server] {
		return nil, errors.New("backend down")
	}
	return []ToolSpec{{Name: "tool_" + server}}, nil
}

func (b *r6Backends) setDown(server string, down bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.down[server] = down
}

func (b *r6Backends) counts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.fetches))
	for k, v := range b.fetches {
		out[k] = v
	}
	return out
}

var r6Targets = []TargetMapping{
	{Server: "a", Tools: map[string]string{"alpha": "tool_a"}},
	{Server: "b", Tools: map[string]string{"beta": "tool_b"}},
}

func r6ToolNames(tools []ToolSpec) string {
	names := make([]string, len(tools))
	for i, tl := range tools {
		names[i] = tl.Name
	}
	return strings.Join(names, ",")
}

// r6FrozenClockTable builds a route table whose clock the test advances by
// hand — the decision input is anchored at the decision point, never slept
// into place.
func r6FrozenClockTable(now *time.Time) *SessionTable {
	tbl := NewSessionTable(8, 30*time.Minute)
	tbl.now = func() time.Time { return *now }
	return tbl
}

// TestR6RouteToolsLoad_DegradedAggregationRefreshes is the finding: backend B
// fails its first fetch, so the first tools/list exposes A only. B recovers.
// Until the refresh point the degraded surface is served from the cache with
// no backend I/O; at the refresh point the next access re-aggregates and
// carries A+B; that complete aggregation is never fetched again.
func TestR6RouteToolsLoad_DegradedAggregationRefreshes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := r6FrozenClockTable(&now)
	sid := tbl.PutRoute("web-search")
	backends := newR6Backends("b")
	load := func() []ToolSpec {
		tools, err := tbl.RouteToolsLoad(context.Background(), sid, func() ([]ToolSpec, bool, error) {
			tools, complete := AggregateTools(r6Targets, backends.fetch)
			return tools, complete, nil
		})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		return tools
	}

	if got := r6ToolNames(load()); got != "alpha" {
		t.Fatalf("first tools/list with backend B down = %q, want A only", got)
	}
	backends.setDown("b", false)

	// One nanosecond short of the refresh point: still the cached surface.
	now = now.Add(routeToolsRefresh - time.Nanosecond)
	if got := r6ToolNames(load()); got != "alpha" {
		t.Fatalf("before the refresh point = %q, want the cached degraded surface", got)
	}
	if got, want := backends.counts(), map[string]int{"a": 1, "b": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backend fetches before the refresh point = %v, want none beyond the first aggregation %v", got, want)
	}

	// At the refresh point: the next access re-aggregates and B is back.
	now = now.Add(time.Nanosecond)
	if got := r6ToolNames(load()); got != "alpha,beta" {
		t.Fatalf("first tools/list past the refresh point = %q, want A+B after the re-aggregation", got)
	}
	if got, want := backends.counts(), map[string]int{"a": 2, "b": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backend fetches after the refresh = %v, want exactly one re-aggregation %v", got, want)
	}

	// The aggregation is complete now: cached for the rest of the session.
	for i := 0; i < 3; i++ {
		now = now.Add(5 * time.Minute)
		if got := r6ToolNames(load()); got != "alpha,beta" {
			t.Fatalf("tools/list %d on a complete cache = %q, want A+B", i+1, got)
		}
	}
	if got, want := backends.counts(), map[string]int{"a": 2, "b": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("complete aggregation was re-fetched: %v, want %v", got, want)
	}
}

// TestR6RouteToolsLoad_StillDegradedRefreshesOncePerWindow: a backend that
// stays down keeps the surface degraded, and the retry cost stays bounded —
// at most one aggregation per refresh window, none in between. An entirely
// failed aggregation (nothing exposed) is short-lived the same way instead
// of pinning an empty tool list on the session.
func TestR6RouteToolsLoad_StillDegradedRefreshesOncePerWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := r6FrozenClockTable(&now)
	sid := tbl.PutRoute("web-search")
	backends := newR6Backends("a", "b")
	load := func() []ToolSpec {
		tools, err := tbl.RouteToolsLoad(context.Background(), sid, func() ([]ToolSpec, bool, error) {
			tools, complete := AggregateTools(r6Targets, backends.fetch)
			return tools, complete, nil
		})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		return tools
	}

	if tools := load(); tools == nil || len(tools) != 0 {
		t.Fatalf("tools/list with every backend down = %v, want an empty (non-nil) list", tools)
	}
	backends.setDown("b", false)

	now = now.Add(routeToolsRefresh)
	if got := r6ToolNames(load()); got != "beta" {
		t.Fatalf("tools/list after B recovered and the window passed = %q, want B", got)
	}
	// Still degraded (A is down): each later window re-aggregates exactly once.
	for i := 0; i < 2; i++ {
		now = now.Add(routeToolsRefresh - time.Nanosecond)
		if got := r6ToolNames(load()); got != "beta" {
			t.Fatalf("inside window %d = %q, want the cached degraded surface", i+1, got)
		}
		if got, want := backends.counts(), map[string]int{"a": 2 + i, "b": 2 + i}; !reflect.DeepEqual(got, want) {
			t.Fatalf("backend fetches inside window %d = %v, want no retries %v", i+1, got, want)
		}
		now = now.Add(time.Nanosecond)
		if got := r6ToolNames(load()); got != "beta" {
			t.Fatalf("at the end of window %d = %q, want the re-aggregated (still degraded) surface", i+1, got)
		}
		if got, want := backends.counts(), map[string]int{"a": 3 + i, "b": 3 + i}; !reflect.DeepEqual(got, want) {
			t.Fatalf("backend fetches at the end of window %d = %v, want exactly one retry %v", i+1, got, want)
		}
	}
}

// TestR6RouteToolsGet_RefreshWindowBoundary pins the policy itself: a
// degraded cache entry is served strictly inside the window and not at its
// end; a complete entry is served for the whole session.
func TestR6RouteToolsGet_RefreshWindowBoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tbl := r6FrozenClockTable(&now)
	sid := tbl.PutRoute("web-search")
	tools := []ToolSpec{{Name: "alpha"}}

	if _, ok := tbl.RouteToolsGet(sid); ok {
		t.Fatal("tools cache should start empty")
	}
	if !tbl.RouteToolsPut(sid, tools, false) {
		t.Fatal("RouteToolsPut failed")
	}
	now = now.Add(routeToolsRefresh - time.Nanosecond)
	if got, ok := tbl.RouteToolsGet(sid); !ok || r6ToolNames(got) != "alpha" {
		t.Fatalf("degraded cache 1ns before the refresh point = %q ok=%v, want served", r6ToolNames(got), ok)
	}
	now = now.Add(time.Nanosecond)
	if got, ok := tbl.RouteToolsGet(sid); ok {
		t.Fatalf("degraded cache at the refresh point = %q, want expired for re-aggregation", r6ToolNames(got))
	}

	if !tbl.RouteToolsPut(sid, tools, true) {
		t.Fatal("RouteToolsPut failed")
	}
	for _, step := range []time.Duration{routeToolsRefresh, 10 * time.Minute, 10 * time.Minute} {
		now = now.Add(step)
		if got, ok := tbl.RouteToolsGet(sid); !ok || r6ToolNames(got) != "alpha" {
			t.Fatalf("complete cache %v into the session = %q ok=%v, want served for the session's lifetime", step, r6ToolNames(got), ok)
		}
	}
}

// TestR6RouteToolsLoad_SingleFlight: concurrent tools/list of one session
// that all miss the cache run exactly ONE aggregation between them — the
// others wait for it and serve its result.
func TestR6RouteToolsLoad_SingleFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tbl := NewSessionTable(8, time.Minute)
		sid := tbl.PutRoute("web-search")
		release := make(chan struct{})
		var aggregations atomic.Int32
		aggregate := func() ([]ToolSpec, bool, error) {
			aggregations.Add(1)
			<-release
			return []ToolSpec{{Name: "alpha"}, {Name: "beta"}}, true, nil
		}
		load := func() []ToolSpec {
			tools, err := tbl.RouteToolsLoad(context.Background(), sid, aggregate)
			if err != nil {
				t.Errorf("tools/list: %v", err)
			}
			return tools
		}

		const callers = 4
		results := make(chan string, callers)
		for i := 0; i < callers; i++ {
			go func() { results <- r6ToolNames(load()) }()
		}
		// Every caller is now parked: the one aggregating on the release
		// gate, the others on its completion.
		synctest.Wait()
		inFlight := aggregations.Load()
		// Open the gate before judging, so a broken single-flight leaves no
		// goroutine parked behind a failed assertion.
		close(release)
		for i := 0; i < callers; i++ {
			select {
			case got := <-results:
				if got != "alpha,beta" {
					t.Errorf("caller %d got %q, want the aggregated surface", i, got)
				}
			case <-time.After(time.Minute):
				t.Fatalf("only %d of %d callers were served", i, callers)
			}
		}
		if inFlight != 1 {
			t.Fatalf("aggregations in flight = %d, want exactly 1 (single-flight)", inFlight)
		}
		if got := aggregations.Load(); got != 1 {
			t.Fatalf("aggregations ran = %d, want exactly 1", got)
		}
		// The winner's result is cached: later calls never aggregate again.
		if got := r6ToolNames(load()); got != "alpha,beta" || aggregations.Load() != 1 {
			t.Fatalf("later call = %q after %d aggregations, want the cached surface", got, aggregations.Load())
		}
	})
}

// TestR6AggregateTools: every target fetched and merged in target order is a
// complete aggregation; a failed fetch drops only that target's canonical
// tools and marks the result degraded; the list is never nil.
func TestR6AggregateTools(t *testing.T) {
	backends := newR6Backends()
	tools, complete := AggregateTools(r6Targets, backends.fetch)
	if names, want := r6ToolNames(tools), "alpha,beta"; !complete || names != want {
		t.Fatalf("complete aggregation = %q complete=%v, want %q complete=true", names, complete, want)
	}

	backends.setDown("a", true)
	tools, complete = AggregateTools(r6Targets, backends.fetch)
	if names, want := r6ToolNames(tools), "beta"; complete || names != want {
		t.Fatalf("degraded aggregation = %q complete=%v, want %q complete=false", names, complete, want)
	}

	backends.setDown("b", true)
	if tools, complete = AggregateTools(r6Targets, backends.fetch); complete || tools == nil || len(tools) != 0 {
		t.Fatalf("fully failed aggregation = %v complete=%v, want an empty non-nil list marked degraded", tools, complete)
	}
}
