package main

import "testing"

// #6 unit: capability resolution order (config caps > catalog > default true).
func TestConvertFixup_ImageOKForTarget(t *testing.T) {
	cat := &modelsDevCatalog{ByName: map[string]modelsDevModel{
		"m-vision": {Input: []string{"text", "image"}},
		"m-text":   {Input: []string{"text"}},
	}}
	tgt := RouteTarget{Provider: "p", Model: "m-vision"}
	// No caps anywhere: catalog decides.
	cfg := &Config{Providers: map[string]Provider{"p": {Provider: "static"}}}
	if !imageOKForTarget(cfg, nil, cat, tgt) {
		t.Error("vision model via catalog must be image-ok")
	}
	if imageOKForTarget(cfg, nil, cat, RouteTarget{Provider: "p", Model: "m-text"}) {
		t.Error("text-only model via catalog must NOT be image-ok")
	}
	// Unknown model: default true (keep reinjecting).
	if !imageOKForTarget(cfg, nil, cat, RouteTarget{Provider: "p", Model: "m-unknown"}) {
		t.Error("unknown model must default to image-ok")
	}
	// Nil catalog: default true.
	if !imageOKForTarget(cfg, nil, nil, tgt) {
		t.Error("nil catalog must default to image-ok")
	}
	// Config capabilities override wins over the catalog both ways.
	cfgCaps := &Config{Providers: map[string]Provider{"p": {Provider: "static", Capabilities: map[string][]string{
		"m-vision": {"text"},          // declared text-only → false despite catalog
		"m-text":   {"text", "image"}, // declared vision → true despite catalog
	}}}}
	if imageOKForTarget(cfgCaps, nil, cat, tgt) {
		t.Error("capabilities override (text-only) must beat catalog vision")
	}
	if !imageOKForTarget(cfgCaps, nil, cat, RouteTarget{Provider: "p", Model: "m-text"}) {
		t.Error("capabilities override (image) must beat catalog text-only")
	}
}
