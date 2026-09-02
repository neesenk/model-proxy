// plan.go — request-to-target planning: per-target plan construction, the
// targetexec executor assembly, and the request-routing planner projection.
package forward

import (
	"fmt"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

// PlanInput is the input to one target plan: the frozen runtime snapshot, the
// route target, and the client protocol/path the request arrived with.
type PlanInput struct {
	Runtime     Snapshot
	Target      RouteTarget
	ClientProto string
	ClientPath  string
}

// planTarget resolves snapshot-owned provider, protocol, endpoint-capability,
// and runtime implementation facts, then freezes them in targetexec.Plan.
func (p pipeline) planTarget(input PlanInput) (targetexec.Plan, error) {
	providerCfg, ok := configdomain.ProviderConfig(input.Runtime.Cfg, input.Runtime.ParentOf, input.Target.Provider)
	if !ok {
		return targetexec.Plan{}, fmt.Errorf("unknown provider %q", input.Target.Provider)
	}
	backendProtoName, viaResponsesVerdict := p.resolvedBackendProto(
		input.Target.Protocol,
		input.Target.Provider,
		providerCfg,
		input.Target.Model,
		input.ClientProto,
		input.Runtime.ParentOf,
	)
	clientProto := protocol.Protocol(input.ClientProto)
	backendProto := protocol.Protocol(backendProtoName)
	imageOK := routing.ImageOKForTarget(
		input.Runtime.Cfg,
		input.Runtime.ParentOf,
		input.Runtime.Catalog,
		input.Target,
	)
	return targetexec.NewPlan(targetexec.PlanInput{
		Target:              input.Target,
		ProviderConfig:      providerCfg,
		Provider:            input.Runtime.Providers[input.Target.Provider],
		ClientProtocol:      clientProto,
		BackendProtocol:     backendProto,
		ViaResponsesVerdict: viaResponsesVerdict,
		ClientPath:          input.ClientPath,
		ImageOK:             imageOK,
		// One collector per target attempt: the attempt's conversion
		// diagnostics ride the plan into the request log; strict refuses
		// lossy conversions for this target when configured.
		Diag:        protocol.NewDiagnostics(),
		StrictLossy: input.Runtime.Cfg.Conversion.StrictLossyValue(),
	}), nil
}

// PlanTarget is the exported plan entry for app's detached branches (Shadow):
// it runs the same snapshot-frozen planning the live pipeline uses.
func PlanTarget(svc Services, input PlanInput) (targetexec.Plan, error) {
	return pipeline{svc: svc}.planTarget(input)
}

// targetExecutor assembles the per-attempt executor. parentOf is the request
// snapshot's pool-virtual→parent projection (Snapshot.ParentOf): it is
// threaded into the health gate so the wire-verdict 404 correction stays on
// the request's own generation (single-snapshot red line).
func (p pipeline) targetExecutor(runtime targetexec.Runtime, parentOf map[string]string) targetexec.Executor {
	return targetexec.Executor{
		Client: p.svc.Client,
		State: targetexec.GateState{
			Gate:       p.svc.NewHealthGate(parentOf),
			Runtime:    runtime,
			Scheduling: runtime.Scheduling,
		},
		Effects:   p.svc.NewEffects(runtime.Generation),
		Responses: p.svc.ResponsesState,
	}
}

// requestRoutingScheduler is the stateful scheduling port used by the
// stateless request-routing planner. Every field belongs to the runtime
// generation captured once at the start of the request; a reload cannot mix
// new config or pool identity into an in-flight cross-route decision.
type requestRoutingScheduler struct {
	pipe       pipeline
	config     *Config
	parentOf   map[string]string
	routeKeys  map[string]bool
	generation uint64
}

func (scheduler requestRoutingScheduler) Schedule(
	routeName, sessionKey string,
	targets []RouteTarget,
) []RouteTarget {
	return scheduler.pipe.schedule(
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
	p pipeline,
	runtime Snapshot,
	routeKeys map[string]bool,
) routing.Planner {
	return routing.NewPlanner(routing.PlannerInput{
		Config:         runtime.Cfg,
		ParentOf:       runtime.ParentOf,
		Catalog:        runtime.Catalog,
		ExpandedRoutes: runtime.ExpandedRoutes,
		Scheduler: requestRoutingScheduler{
			pipe:       p,
			config:     runtime.Cfg,
			parentOf:   runtime.ParentOf,
			routeKeys:  routeKeys,
			generation: runtime.Generation,
		},
	})
}
