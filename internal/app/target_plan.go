package app

import (
	"fmt"
	configdomain "model-proxy/internal/config"

	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

type targetPlanInput struct {
	runtime     RuntimeSnapshot
	target      RouteTarget
	clientProto string
	clientPath  string
}

// planTarget resolves snapshot-owned provider, protocol, endpoint-capability,
// and runtime implementation facts, then freezes them in targetexec.Plan.
func (p *Proxy) planTarget(input targetPlanInput) (targetexec.Plan, error) {
	providerCfg, ok := configdomain.ProviderConfig(input.runtime.Cfg, input.runtime.ParentOf, input.target.Provider)
	if !ok {
		return targetexec.Plan{}, fmt.Errorf("unknown provider %q", input.target.Provider)
	}
	backendProtoName, viaResponsesVerdict := p.resolvedBackendProto(
		input.target.Protocol,
		input.target.Provider,
		providerCfg,
		input.target.Model,
		input.clientProto,
		input.runtime.ParentOf,
	)
	clientProto := protocol.Protocol(input.clientProto)
	backendProto := protocol.Protocol(backendProtoName)
	imageOK := routing.ImageOKForTarget(
		input.runtime.Cfg,
		input.runtime.ParentOf,
		input.runtime.Catalog,
		input.target,
	)
	return targetexec.NewPlan(targetexec.PlanInput{
		Target:              input.target,
		ProviderConfig:      providerCfg,
		Provider:            input.runtime.Providers[input.target.Provider],
		ClientProtocol:      clientProto,
		BackendProtocol:     backendProto,
		ViaResponsesVerdict: viaResponsesVerdict,
		ClientPath:          input.clientPath,
		ImageOK:             imageOK,
		// One collector per target attempt: the attempt's conversion
		// diagnostics ride the plan into the request log; strict refuses
		// lossy conversions for this target when configured.
		Diag:        protocol.NewDiagnostics(),
		StrictLossy: input.runtime.Cfg.Conversion.StrictLossyValue(),
	}), nil
}
