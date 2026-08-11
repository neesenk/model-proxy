package app

import (
	"net/http"

	"model-proxy/internal/routing"
)

// forcedProviderFromRequest extracts the HTTP boundary value used by replay.
// Header wins over query. The policy package receives only the resulting value.
func forcedProviderFromRequest(request *http.Request) string {
	if request == nil {
		return ""
	}
	if value := request.Header.Get("x-mp-force-provider"); value != "" {
		return value
	}
	if request.URL == nil {
		return ""
	}
	return request.URL.Query().Get("force_provider")
}

// requestRoutingScheduler is the stateful scheduling port used by the
// stateless request-routing planner. Every field belongs to the runtime
// generation captured once at the start of forward; a reload cannot mix new
// config or pool identity into an in-flight cross-route decision.
type requestRoutingScheduler struct {
	proxy      *Proxy
	config     *Config
	parentOf   map[string]string
	routeKeys  map[string]bool
	generation uint64
}

func (scheduler requestRoutingScheduler) Schedule(
	routeName, sessionKey string,
	targets []RouteTarget,
) []RouteTarget {
	return scheduler.proxy.schedule(
		scheduler.config,
		scheduler.parentOf,
		routeName,
		sessionKey,
		targets,
		scheduler.routeKeys,
		scheduler.generation,
	)
}

// requestRoutingPlanner projects one immutable runtime snapshot into the pure
// policy package and binds only the narrow scheduler port that may mutate
// sticky/round-robin state.
func requestRoutingPlanner(
	proxy *Proxy,
	runtime RuntimeSnapshot,
	routeKeys map[string]bool,
) routing.Planner {
	return routing.NewPlanner(routing.PlannerInput{
		Config:         runtime.Cfg,
		ParentOf:       runtime.ParentOf,
		Catalog:        runtime.Catalog,
		ExpandedRoutes: runtime.ExpandedRoutes,
		Scheduler: requestRoutingScheduler{
			proxy:      proxy,
			config:     runtime.Cfg,
			parentOf:   runtime.ParentOf,
			routeKeys:  routeKeys,
			generation: runtime.Generation,
		},
	})
}
