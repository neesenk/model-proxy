package main

import (
	"fmt"

	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

type targetPlanInput struct {
	runtime     runtimeSnapshot
	target      RouteTarget
	clientProto string
	clientPath  string
}

// planTarget resolves snapshot-owned provider, protocol, endpoint-capability,
// and runtime implementation facts, then freezes them in targetexec.Plan.
func (p *Proxy) planTarget(input targetPlanInput) (targetexec.Plan, error) {
	providerCfg, ok := providerConfig(input.runtime.cfg, input.runtime.parentOf, input.target.Provider)
	if !ok {
		return targetexec.Plan{}, fmt.Errorf("unknown provider %q", input.target.Provider)
	}
	backendProtoName, viaResponsesVerdict := p.resolvedBackendProto(
		input.target.Protocol,
		input.target.Provider,
		providerCfg,
		input.target.Model,
		input.clientProto,
		input.runtime.parentOf,
	)
	clientProto := protocol.Protocol(input.clientProto)
	backendProto := protocol.Protocol(backendProtoName)
	imageOK := routing.ImageOKForTarget(
		input.runtime.cfg,
		input.runtime.parentOf,
		input.runtime.catalog,
		input.target,
	)
	return targetexec.NewPlan(targetexec.PlanInput{
		Target:              input.target,
		ProviderConfig:      providerCfg,
		Provider:            input.runtime.providers[input.target.Provider],
		ClientProtocol:      clientProto,
		BackendProtocol:     backendProto,
		ViaResponsesVerdict: viaResponsesVerdict,
		ClientPath:          input.clientPath,
		ImageOK:             imageOK,
	}), nil
}
