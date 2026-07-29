package main

import (
	"fmt"

	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
	"model-proxy/provider"
)

// targetPlan resolves root-owned provider/runtime facts for one RouteTarget.
// Its wire preparation is delegated to targetexec.Plan so normal routing and
// every Fusion leg share one immutable conversion contract.
type targetPlan struct {
	target              RouteTarget
	providerCfg         Provider
	providerImpl        provider.Provider
	clientProto         protocol.Protocol
	backendProto        protocol.Protocol
	viaResponsesVerdict bool
	baseURL             string
	upPath              string
	imageOK             bool
	wire                targetexec.Plan
}

type targetPlanInput struct {
	runtime     runtimeSnapshot
	target      RouteTarget
	clientProto string
	clientPath  string
}

func (p *Proxy) planTarget(input targetPlanInput) (targetPlan, error) {
	providerCfg, ok := providerConfig(input.runtime.cfg, input.runtime.parentOf, input.target.Provider)
	if !ok {
		return targetPlan{}, fmt.Errorf("unknown provider %q", input.target.Provider)
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
	imageOK := imageOKForTarget(
		input.runtime.cfg,
		input.runtime.parentOf,
		input.runtime.catalog,
		input.target,
	)
	wirePlan := targetexec.NewPlan(targetexec.PlanInput{
		TargetModel:      input.target.Model,
		ProviderID:       providerCfg.Provider,
		ClientProtocol:   clientProto,
		BackendProtocol:  backendProto,
		OpenAIBaseURL:    providerCfg.OpenAIBaseURL,
		AnthropicBaseURL: providerCfg.AnthropicBaseURL,
		ClientPath:       input.clientPath,
		ImageOK:          imageOK,
	})
	return targetPlan{
		target:              input.target,
		providerCfg:         providerCfg,
		providerImpl:        input.runtime.providers[input.target.Provider],
		clientProto:         clientProto,
		backendProto:        backendProto,
		viaResponsesVerdict: viaResponsesVerdict,
		baseURL:             wirePlan.BaseURL(),
		upPath:              wirePlan.UpstreamPath(),
		imageOK:             imageOK,
		wire:                wirePlan,
	}, nil
}
