package mcp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// r6_route_tools_abort_test.go pins when an aggregation is NOT a snapshot of
// the route and must never reach the session's tools cache (R6 review, group
// C): one that was aborted by a fail-closed condition, and one that finished
// after its caller had already left. Either would otherwise be served — as a
// "degraded" surface missing a backend — for a whole refresh window.

var errR6StaleBinding = errors.New("a backend binding was stale")

// TestR6RouteToolsLoad_AbortedAggregationNotCached: an aggregation that
// reports an error was aborted — it says nothing about the route's surface.
// Its result is neither cached nor returned as tools, the callers that waited
// behind it aggregate for themselves instead of being served it, and the next
// caller's aggregation is the one that ends up cached.
func TestR6RouteToolsLoad_AbortedAggregationNotCached(t *testing.T) {
	tbl := NewSessionTable(8, time.Minute)
	sid := tbl.PutRoute("web-search")
	calls := 0
	aggregate := func() ([]ToolSpec, bool, error) {
		calls++
		if calls == 1 {
			return nil, false, errR6StaleBinding // hit a stale backend binding
		}
		return []ToolSpec{{Name: "alpha"}, {Name: "beta"}}, true, nil
	}

	tools, err := tbl.RouteToolsLoad(context.Background(), sid, aggregate)
	if !errors.Is(err, errR6StaleBinding) || tools != nil {
		t.Fatalf("aborted aggregation = %q, %v; want the abort reported and no tools", r6ToolNames(tools), err)
	}
	// Nothing reached the cache: the next caller aggregates afresh.
	tools, err = tbl.RouteToolsLoad(context.Background(), sid, aggregate)
	if err != nil || calls != 2 || r6ToolNames(tools) != "alpha,beta" {
		t.Fatalf("load after the abort = %q, %v after %d aggregations; want a fresh aggregation's complete surface", r6ToolNames(tools), err, calls)
	}
	// That one is complete, so it is cached for the rest of the session.
	tools, err = tbl.RouteToolsLoad(context.Background(), sid, aggregate)
	if err != nil || calls != 2 || r6ToolNames(tools) != "alpha,beta" {
		t.Fatalf("load after the fresh aggregation = %q, %v after %d aggregations; want the cached surface", r6ToolNames(tools), err, calls)
	}
}

// TestR6RouteToolsLoad_AbortedAggregationWaitersReaggregate: with callers
// queued behind an aggregation that aborts, none of them is served the
// aborted result — exactly one of them aggregates again and the others share
// that complete surface.
func TestR6RouteToolsLoad_AbortedAggregationWaitersReaggregate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tbl := NewSessionTable(8, time.Minute)
		sid := tbl.PutRoute("web-search")
		gate := make(chan struct{})
		var aggregations atomic.Int32
		aggregate := func() ([]ToolSpec, bool, error) {
			if aggregations.Add(1) == 1 {
				<-gate // the first aggregation stays in flight while the others line up behind it
				return nil, false, errR6StaleBinding
			}
			return []ToolSpec{{Name: "alpha"}, {Name: "beta"}}, true, nil
		}

		const callers = 4
		results := make(chan string, callers)
		for i := 0; i < callers; i++ {
			go func() {
				tools, err := tbl.RouteToolsLoad(context.Background(), sid, aggregate)
				if err != nil {
					results <- "error: " + err.Error()
					return
				}
				results <- r6ToolNames(tools)
			}()
		}
		// One caller is parked in the (about to abort) aggregation, the other
		// three on its completion.
		synctest.Wait()
		close(gate)
		var complete int
		for i := 0; i < callers; i++ {
			select {
			case got := <-results:
				if got == "alpha,beta" {
					complete++
				} else if got != "error: a backend binding was stale" {
					t.Errorf("caller %d got %q, want a complete surface or the abort", i, got)
				}
			case <-time.After(time.Minute):
				t.Fatalf("only %d of %d callers returned", i, callers)
			}
		}
		if complete != callers-1 {
			t.Fatalf("%d of %d callers got a complete surface, want everyone but the one whose aggregation aborted", complete, callers)
		}
		// The aborting aggregation plus exactly one re-aggregation.
		if got := aggregations.Load(); got != 2 {
			t.Fatalf("aggregations ran = %d, want 2 (the aborted one and one re-aggregation)", got)
		}
	})
}

// TestR6RouteToolsLoad_DepartedCallerResultNotCached: an aggregation that
// completes after its caller left is not cached. Every backend "failed" only
// because nobody was waiting for the answer; serving that to the session for a
// refresh window would hide the route's tools. The departed caller is still
// handed its result — it is not an abort.
func TestR6RouteToolsLoad_DepartedCallerResultNotCached(t *testing.T) {
	tbl := NewSessionTable(8, time.Minute)
	sid := tbl.PutRoute("web-search")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inside := make(chan struct{})
	release := make(chan struct{})
	departed := make(chan error, 1)
	go func() {
		_, err := tbl.RouteToolsLoad(ctx, sid, func() ([]ToolSpec, bool, error) {
			close(inside)
			<-release
			return []ToolSpec{}, false, nil
		})
		departed <- err
	}()

	<-inside       // the aggregation is in flight
	cancel()       // its caller disconnects
	close(release) // and the aggregation runs out with nobody waiting
	if err := <-departed; err != nil {
		t.Fatalf("the departed caller's load = %v, want its result without an abort", err)
	}

	// The result was not cached: the next caller aggregates for itself.
	fresh := 0
	tools, err := tbl.RouteToolsLoad(context.Background(), sid, func() ([]ToolSpec, bool, error) {
		fresh++
		return []ToolSpec{{Name: "alpha"}, {Name: "beta"}}, true, nil
	})
	if err != nil || fresh != 1 || r6ToolNames(tools) != "alpha,beta" {
		t.Fatalf("load after the departed caller = %q, %v after %d fresh aggregations; want its own complete surface", r6ToolNames(tools), err, fresh)
	}
}
