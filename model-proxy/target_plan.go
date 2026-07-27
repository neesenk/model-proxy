package main

import (
	"fmt"

	"model-proxy/provider"
)

// targetPlan is the immutable wire plan for one resolved RouteTarget. Normal
// routing and every Fusion leg share this preparation step so provider lookup,
// backend protocol selection, body conversion, and upstream path selection
// cannot drift into parallel implementations.
type targetPlan struct {
	target              RouteTarget
	providerCfg         Provider
	providerImpl        provider.Provider
	clientProto         string
	backendProto        string
	viaResponsesVerdict bool
	baseURL             string
	upPath              string
	imageOK             bool
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
	backendProto, viaResponsesVerdict := p.resolvedBackendProto(
		input.target.Protocol,
		input.target.Provider,
		providerCfg,
		input.target.Model,
		input.clientProto,
		input.runtime.parentOf,
	)
	convert := needsConversion(input.clientProto, backendProto)
	baseURL := providerCfg.OpenAIBaseURL
	if backendProto == "anthropic" && providerCfg.AnthropicBaseURL != "" {
		baseURL = providerCfg.AnthropicBaseURL
	}
	upPath := input.clientPath
	if convert {
		upPath = backendPath(backendProto)
	}
	return targetPlan{
		target:              input.target,
		providerCfg:         providerCfg,
		providerImpl:        input.runtime.providers[input.target.Provider],
		clientProto:         input.clientProto,
		backendProto:        backendProto,
		viaResponsesVerdict: viaResponsesVerdict,
		baseURL:             baseURL,
		upPath:              upPath,
		imageOK: imageOKForTarget(
			input.runtime.cfg,
			input.runtime.parentOf,
			input.runtime.catalog,
			input.target,
		),
	}, nil
}

func (plan targetPlan) rewriteModel(body []byte, calledModel string) []byte {
	// Shadow targets may declare no model (pass the called model through);
	// route targets always carry one.
	if plan.target.Model == "" || plan.target.Model == calledModel {
		return body
	}
	return rewriteModel(body, plan.target.Model)
}

func (plan targetPlan) convertBody(body []byte) ([]byte, error) {
	if !needsConversion(plan.clientProto, plan.backendProto) {
		return body, nil
	}
	return convertRequestFor(body, plan.clientProto, plan.backendProto, convertReqOpts{
		ProviderID: plan.providerCfg.Provider,
		ImageOK:    plan.imageOK,
	})
}
