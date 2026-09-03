package routing

import (
	"reflect"
	"strings"
	"testing"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

func testCatalog(entries map[string]struct {
	Context int64
	Input   []string
}, tools ...string) *catalog.Catalog {
	byName := make(map[string]catalog.Model, len(entries))
	for name, entry := range entries {
		byName[name] = catalog.Model{
			Context: entry.Context,
			Modalities: catalog.Modalities{
				Input: entry.Input,
			},
		}
	}
	for _, name := range tools {
		model := byName[name]
		model.ToolCall = true
		byName[name] = model
	}
	return catalog.New(byName)
}

func TestModelFitsRequest(t *testing.T) {
	cat := testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text":   {Context: 8000, Input: []string{"text"}},
		"vision": {Context: 8000, Input: []string{"text", "image"}},
		"big":    {Context: 128000, Input: []string{"text"}},
	}, "vision")
	image := []byte(`{"messages":[{"content":[{"type":"image"}]}]}`)
	text := []byte(`{"messages":[{"content":"hi"}]}`)
	big := []byte(`{"input":"` + strings.Repeat("qwxz!", 8000) + `"}`)
	tools := []byte(`{"messages":[{"content":"hi"}],"tools":[{"name":"f"}]}`)

	if !Fits(cat, nil, "text", ProfileRequest(text)) {
		t.Error("text model + text request should fit")
	}
	if Fits(cat, nil, "text", ProfileRequest(image)) {
		t.Error("text model + image request should NOT fit")
	}
	if Fits(cat, nil, "text", ProfileRequest(big)) {
		t.Error("text model + big request should NOT fit (exceeds 8000)")
	}
	if !Fits(cat, nil, "vision", ProfileRequest(image)) {
		t.Error("vision model + image request should fit")
	}
	if Fits(cat, nil, "vision", ProfileRequest(big)) {
		t.Error("vision model + big request should NOT fit (exceeds 8000)")
	}
	if !Fits(cat, nil, "big", ProfileRequest(big)) {
		t.Error("big model + big request should fit")
	}
	if !Fits(cat, nil, "vision", ProfileRequest(tools)) {
		t.Error("tools-capable model + tools request should fit")
	}
	if Fits(cat, nil, "text", ProfileRequest(tools)) {
		t.Error("tool-blind model + tools request should NOT fit")
	}
	if Fits(cat, nil, "unknown", ProfileRequest(tools)) {
		t.Error("unknown model + tools request should NOT fit")
	}

	exact := []byte(`{"input":"` + strings.Repeat("qwxz!", 6398) + `"}`)
	if !Fits(cat, nil, "text", ProfileRequest(exact)) {
		t.Error("est == context window should fit")
	}
	if !Fits(nil, nil, "anything", ProfileRequest(image)) {
		t.Error("nil catalog should fit")
	}
	if Fits(cat, nil, "unknown", ProfileRequest(image)) {
		t.Error("unknown model + image should NOT fit")
	}
	if !Fits(cat, nil, "unknown", ProfileRequest(big)) {
		t.Error("unknown model + unknown context should fit")
	}
}

func TestModelFits_CapabilitiesOverride(t *testing.T) {
	cat := testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text": {Context: 8000, Input: []string{"text"}},
	})
	capabilities := map[string][]string{
		"blind":       {"image"},
		"blind-tools": {"tools"},
		"empty":       {},
	}
	image := ProfileRequest([]byte(`{"messages":[{"content":[{"type":"image"}]}]}`))
	tools := ProfileRequest([]byte(`{"tools":[{"name":"x"}]}`))
	text := ProfileRequest([]byte(`{"messages":[{"content":"hi"}]}`))

	if !Fits(cat, capabilities, "blind", image) {
		t.Error("declared [image] blind model + image request should fit")
	}
	if Fits(cat, capabilities, "blind", tools) {
		t.Error("declared [image] model + tools request should NOT fit")
	}
	if !Fits(cat, capabilities, "blind-tools", tools) {
		t.Error("declared [tools] blind model + tools request should fit")
	}
	if Fits(cat, capabilities, "blind-tools", image) {
		t.Error("declared [tools] model + image request should NOT fit")
	}
	if Fits(cat, capabilities, "empty", image) || Fits(cat, capabilities, "empty", tools) {
		t.Error("declared [] model should fit neither image nor tools requests")
	}
	if !Fits(cat, capabilities, "empty", text) {
		t.Error("declared [] model + plain text request should fit")
	}
	if Fits(cat, capabilities, "text", image) {
		t.Error("undeclared text-only model + image request should NOT fit")
	}
	if Fits(cat, capabilities, "unknown", image) {
		t.Error("undeclared unknown model + image request should NOT fit")
	}
	if !Fits(cat, capabilities, "unknown", text) {
		t.Error("undeclared unknown model + text request should fit")
	}
}

func TestTargetCapabilities_PooledVirtual(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"zhipu": {
			Capabilities: map[string][]string{
				"glm-x": {"image"},
			},
		},
	}}
	parentOf := map[string]string{"zhipu#abc123": "zhipu"}
	virtual := configdomain.RouteTarget{Provider: "zhipu#abc123", Model: "glm-x"}
	if got := CapabilitiesFor(cfg, parentOf, virtual)["glm-x"]; !reflect.DeepEqual(got, []string{"image"}) {
		t.Fatalf("virtual capabilities = %v, want [image]", got)
	}
	direct := configdomain.RouteTarget{Provider: "zhipu", Model: "glm-x"}
	if got := CapabilitiesFor(cfg, parentOf, direct)["glm-x"]; !reflect.DeepEqual(got, []string{"image"}) {
		t.Fatalf("direct capabilities = %v, want [image]", got)
	}
	if got := CapabilitiesFor(cfg, parentOf, configdomain.RouteTarget{Provider: "unknown"}); got != nil {
		t.Fatalf("unknown capabilities = %v, want nil", got)
	}
	if got := CapabilitiesFor(nil, parentOf, direct); got != nil {
		t.Fatalf("nil-config capabilities = %v, want nil", got)
	}
}

func TestCollectCrossRoute_DedupIgnoresPriority(t *testing.T) {
	expanded := map[string][]configdomain.RouteTarget{
		"routeA": {{Provider: "zhipu", Model: "glm", Protocol: "openai", Priority: 1}},
		"routeB": {{Provider: "zhipu", Model: "glm", Protocol: "openai", Priority: 3}},
		"routeC": {{Provider: "deepseek", Model: "ds", Protocol: "openai", Priority: 2}},
		"routeD": {{Provider: "zhipu#acct-b", Model: "glm", Protocol: "openai", Priority: 1}},
		"routeE": {{Provider: "fusion", Model: "recipe", Priority: 0}},
	}
	pool := CollectCrossRoute(expanded, func(configdomain.RouteTarget) bool { return true })
	if len(pool) != 3 {
		t.Fatalf("pool has %d targets, want 3: %+v", len(pool), pool)
	}
	var zhipu *configdomain.RouteTarget
	for index := range pool {
		if pool[index].Provider == "zhipu" && pool[index].Model == "glm" {
			zhipu = &pool[index]
		}
	}
	if zhipu == nil {
		t.Fatal("zhipu/glm/openai missing")
	}
	if zhipu.Priority != 1 {
		t.Errorf("deduped priority = %d, want 1", zhipu.Priority)
	}
}

func TestCollectCrossRoute_NilKeepMeansAll(t *testing.T) {
	expanded := map[string][]configdomain.RouteTarget{
		"routeA": {
			{Provider: "zhipu", Model: "glm", Protocol: "openai", Priority: 1},
			{Provider: "fusion", Model: "recipe"},
		},
		"routeB": {
			{Provider: "deepseek", Model: "ds", Protocol: "openai", Priority: 2},
		},
	}
	got := CollectCrossRoute(expanded, nil)
	if len(got) != 2 {
		t.Fatalf("nil keep collected %d targets, want 2 concrete targets: %+v", len(got), got)
	}
	seen := make(map[string]bool, len(got))
	for _, target := range got {
		seen[target.Provider+"/"+target.Model+"/"+target.Protocol] = true
	}
	want := map[string]bool{
		"zhipu/glm/openai":   true,
		"deepseek/ds/openai": true,
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("nil keep identities = %#v, want %#v", seen, want)
	}
}

func TestEstimateInputTokens(t *testing.T) {
	if got := EstimateInputTokens([]byte("abcd")); got != 1 {
		t.Errorf("ASCII estimate = %d, want 1", got)
	}
	if got := EstimateInputTokens([]byte("你好世界")); got != 4 {
		t.Errorf("CJK estimate = %d, want 4", got)
	}
	body := []byte(`{"image":"` + strings.Repeat("A", 128) + `"}`)
	if got := EstimateInputTokens(body); got != int64(len(`{"image":""}`)/4) {
		t.Errorf("base64 estimate = %d, want wrapper-only %d", got, len(`{"image":""}`)/4)
	}
}

// TestEstimateInputTokens_RuneAware preserves the original routing estimator
// contract: ASCII uses the /4 heuristic, CJK is rune-aware, and long base64
// payloads do not dominate the context estimate.
func TestEstimateInputTokens_RuneAware(t *testing.T) {
	if estimated := EstimateInputTokens([]byte(`{"model":"abc"}`)); estimated < 3 || estimated > 5 {
		t.Errorf("ASCII: est=%d want ~4", estimated)
	}
	cjk := []byte(`{"input":"你好世界"}`)
	estimated := EstimateInputTokens(cjk)
	if estimated < 5 || estimated > 10 {
		t.Errorf("CJK: est=%d want ~7 (4 CJK + ~3 ASCII)", estimated)
	}
	withBase64 := []byte(`{"image":"` + strings.Repeat("A", 200) + `","text":"hi"}`)
	if estimated = EstimateInputTokens(withBase64); estimated > 15 {
		t.Errorf("base64 not excluded: est=%d want <15", estimated)
	}
}

// TestRequestHasTools preserves both the positive tools-array shape and the
// exact negative control from the former root-package pure test.
func TestRequestHasTools(t *testing.T) {
	if !RequestHasTools([]byte(`{"tools":[{"type":"function"}]}`)) {
		t.Error("body with tools:[ should be detected")
	}
	if RequestHasTools([]byte(`{"model":"x","input":[]}`)) {
		t.Error("body without tools should not match")
	}
}

// TestDecodeRune_MultiByte keeps direct coverage of the estimator's custom
// decoder and invalid-byte fallback.
func TestDecodeRune_MultiByte(t *testing.T) {
	decoded, size := decodeRune([]byte{0xE5, 0xA5, 0xBD}, 0)
	if decoded != 0x597D || size != 3 {
		t.Errorf("CJK decode: r=%X size=%d want 597D/3", decoded, size)
	}
	if !isCJK(0x597D) {
		t.Error("U+597D should be CJK")
	}
	_, invalidSize := decodeRune([]byte{0x80}, 0)
	if invalidSize != 1 {
		t.Errorf("invalid byte: size=%d want 1", invalidSize)
	}
}

func TestFilterTargetsByProvider(t *testing.T) {
	targets := []configdomain.RouteTarget{
		{Provider: "zhipu#a", Model: "glm"},
		{Provider: "zhipu#b", Model: "glm"},
		{Provider: "deepseek", Model: "ds"},
	}
	parentOf := map[string]string{"zhipu#a": "zhipu", "zhipu#b": "zhipu"}
	if got := FilterTargetsByProvider(targets, parentOf, "zhipu"); !reflect.DeepEqual(got, targets[:2]) {
		t.Fatalf("parent filter = %+v, want %+v", got, targets[:2])
	}
	if got := FilterTargetsByProvider(targets, parentOf, "zhipu#a"); !reflect.DeepEqual(got, targets[:1]) {
		t.Fatalf("virtual filter = %+v, want %+v", got, targets[:1])
	}
	if got := FilterTargetsByProvider(targets, parentOf, "missing"); len(got) != 0 {
		t.Fatalf("missing filter = %+v, want empty", got)
	}
}

func TestSplitProviderPrefix(t *testing.T) {
	providers := map[string]configdomain.Provider{
		"deepseek": {},
		"zhipu":    {},
	}
	cases := []struct {
		called          string
		provider, model string
		ok              bool
	}{
		{"deepseek/deepseek-v4-pro", "deepseek", "deepseek-v4-pro", true},
		{"zhipu/openai/gpt-5", "zhipu", "openai/gpt-5", true}, // model may contain "/"
		{"openai/gpt-5", "", "", false},                       // prefix not a provider
		{"deepseek-v4-pro", "", "", false},                    // no slash
		{"deepseek/", "", "", false},                          // empty model
		{"/glm", "", "", false},                               // empty provider
	}
	for _, tc := range cases {
		p, m, ok := SplitProviderPrefix(providers, tc.called)
		if ok != tc.ok || p != tc.provider || m != tc.model {
			t.Errorf("SplitProviderPrefix(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.called, p, m, ok, tc.provider, tc.model, tc.ok)
		}
	}
}

type scheduleCall struct {
	routeName string
	session   string
	targets   []configdomain.RouteTarget
}

type recordingScheduler struct {
	calls  []scheduleCall
	result []configdomain.RouteTarget
}

func (scheduler *recordingScheduler) Schedule(
	routeName, sessionKey string,
	targets []configdomain.RouteTarget,
) []configdomain.RouteTarget {
	scheduler.calls = append(scheduler.calls, scheduleCall{
		routeName: routeName,
		session:   sessionKey,
		targets:   append([]configdomain.RouteTarget(nil), targets...),
	})
	return append([]configdomain.RouteTarget(nil), scheduler.result...)
}

func TestPlannerApply(t *testing.T) {
	cat := testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text":   {Context: 8000, Input: []string{"text"}},
		"vision": {Context: 32000, Input: []string{"text", "image"}},
	})
	text := configdomain.RouteTarget{Provider: "text-p", Model: "text", Priority: 1}
	vision := configdomain.RouteTarget{Provider: "vision-p", Model: "vision", Priority: 2}
	fusion := configdomain.RouteTarget{Provider: "fusion", Model: "recipe"}
	imageBody := []byte(`{"messages":[{"content":[{"type":"image"}]}]}`)

	t.Run("nil catalog is no-op", func(t *testing.T) {
		scheduler := &recordingScheduler{}
		ordered := []configdomain.RouteTarget{text}
		planner := NewPlanner(PlannerInput{Scheduler: scheduler})
		got := planner.ApplyWithProfile("route", "session", ordered, ProfileRequest(imageBody))
		if !reflect.DeepEqual(got, ordered) || len(scheduler.calls) != 0 {
			t.Fatalf("got=%+v calls=%+v, want original and no scheduling", got, scheduler.calls)
		}
	})

	t.Run("partial in-route match preserves order without scheduling", func(t *testing.T) {
		scheduler := &recordingScheduler{}
		planner := NewPlanner(PlannerInput{Catalog: cat, Scheduler: scheduler})
		got := planner.ApplyWithProfile(
			"route",
			"session",
			[]configdomain.RouteTarget{text, vision},
			ProfileRequest(imageBody),
		)
		if !reflect.DeepEqual(got, []configdomain.RouteTarget{vision}) {
			t.Fatalf("filtered = %+v, want vision only", got)
		}
		if len(scheduler.calls) != 0 {
			t.Fatalf("scheduler calls = %+v, want none", scheduler.calls)
		}
	})

	t.Run("fusion always fits", func(t *testing.T) {
		scheduler := &recordingScheduler{}
		planner := NewPlanner(PlannerInput{Catalog: cat, Scheduler: scheduler})
		got := planner.ApplyWithProfile(
			"route",
			"session",
			[]configdomain.RouteTarget{text, fusion},
			ProfileRequest(imageBody),
		)
		if !reflect.DeepEqual(got, []configdomain.RouteTarget{fusion}) {
			t.Fatalf("filtered = %+v, want fusion only", got)
		}
		if len(scheduler.calls) != 0 {
			t.Fatalf("scheduler calls = %+v, want none", scheduler.calls)
		}
	})

	t.Run("cross-route uses exact synthetic key and scheduler result", func(t *testing.T) {
		scheduler := &recordingScheduler{result: []configdomain.RouteTarget{vision}}
		planner := NewPlanner(PlannerInput{
			Catalog: cat,
			ExpandedRoutes: map[string][]configdomain.RouteTarget{
				"route":        {text},
				"vision-route": {vision},
				"fusion":       {fusion},
			},
			Scheduler: scheduler,
		})
		got := planner.ApplyWithProfile("public", "session-1", []configdomain.RouteTarget{text}, ProfileRequest(imageBody))
		if !reflect.DeepEqual(got, []configdomain.RouteTarget{vision}) {
			t.Fatalf("scheduled = %+v, want vision", got)
		}
		if len(scheduler.calls) != 1 {
			t.Fatalf("scheduler calls = %d, want 1", len(scheduler.calls))
		}
		call := scheduler.calls[0]
		if call.routeName != "public#req" || call.session != "session-1" {
			t.Fatalf("scheduler identity = %q/%q, want public#req/session-1", call.routeName, call.session)
		}
		if !reflect.DeepEqual(call.targets, []configdomain.RouteTarget{vision}) {
			t.Fatalf("scheduler candidates = %+v, want vision only", call.targets)
		}
	})

	t.Run("empty scheduled fallback returns original", func(t *testing.T) {
		scheduler := &recordingScheduler{}
		ordered := []configdomain.RouteTarget{text}
		planner := NewPlanner(PlannerInput{
			Catalog:        cat,
			ExpandedRoutes: map[string][]configdomain.RouteTarget{"vision": {vision}},
			Scheduler:      scheduler,
		})
		got := planner.ApplyWithProfile("public", "", ordered, ProfileRequest(imageBody))
		if !reflect.DeepEqual(got, ordered) {
			t.Fatalf("empty fallback = %+v, want original %+v", got, ordered)
		}
		if len(scheduler.calls) != 1 {
			t.Fatalf("scheduler calls = %d, want 1", len(scheduler.calls))
		}
	})
}

func TestPlannerContextOverflowRetry(t *testing.T) {
	cat := testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"small":       {Context: 8000, Input: []string{"text", "image"}},
		"equal":       {Context: 8000, Input: []string{"text", "image"}},
		"large":       {Context: 32000, Input: []string{"text", "image"}},
		"large-blind": {Context: 64000, Input: []string{"text"}},
		"unknown":     {Context: 0, Input: []string{"text", "image"}},
	})
	small := configdomain.RouteTarget{Provider: "small-p", Model: "small"}
	equal := configdomain.RouteTarget{Provider: "equal-p", Model: "equal"}
	large := configdomain.RouteTarget{Provider: "large-p", Model: "large"}
	largeBlind := configdomain.RouteTarget{Provider: "blind-p", Model: "large-blind"}
	unknown := configdomain.RouteTarget{Provider: "unknown-p", Model: "unknown"}
	imageBody := []byte(`{"messages":[{"content":[{"type":"image"}]}]}`)

	scheduler := &recordingScheduler{result: []configdomain.RouteTarget{large}}
	planner := NewPlanner(PlannerInput{
		Catalog: cat,
		ExpandedRoutes: map[string][]configdomain.RouteTarget{
			"equal":   {equal},
			"large":   {large},
			"blind":   {largeBlind},
			"unknown": {unknown},
		},
		Scheduler: scheduler,
	})
	got := planner.ContextOverflowRetryWithProfile("public", "session-2", []configdomain.RouteTarget{small}, ProfileRequest(imageBody))
	if !reflect.DeepEqual(got, []configdomain.RouteTarget{large}) {
		t.Fatalf("retry = %+v, want large", got)
	}
	if len(scheduler.calls) != 1 {
		t.Fatalf("scheduler calls = %d, want 1", len(scheduler.calls))
	}
	call := scheduler.calls[0]
	if call.routeName != "public#ctx" || call.session != "session-2" {
		t.Fatalf("scheduler identity = %q/%q, want public#ctx/session-2", call.routeName, call.session)
	}
	if !reflect.DeepEqual(call.targets, []configdomain.RouteTarget{large}) {
		t.Fatalf("retry candidates = %+v, want only strictly-larger capable target", call.targets)
	}

	scheduler.calls = nil
	got = planner.ContextOverflowRetryWithProfile(
		"public",
		"session-2",
		[]configdomain.RouteTarget{{Provider: "missing", Model: "missing"}},
		ProfileRequest(imageBody),
	)
	if got != nil || len(scheduler.calls) != 0 {
		t.Fatalf("unknown tried context: got=%+v calls=%+v, want nil/no call", got, scheduler.calls)
	}
}

func TestImageOKForTarget(t *testing.T) {
	cat := testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text":   {Context: 8000, Input: []string{"text"}},
		"vision": {Context: 8000, Input: []string{"text", "image"}},
	})
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"declared": {
			Capabilities: map[string][]string{
				"blind-vision": {"image"},
				"blind-text":   {},
			},
		},
	}}
	if !ImageOKForTarget(cfg, nil, cat, configdomain.RouteTarget{Provider: "declared", Model: "blind-vision"}) {
		t.Error("declared image target should preserve images")
	}
	if ImageOKForTarget(cfg, nil, cat, configdomain.RouteTarget{Provider: "declared", Model: "blind-text"}) {
		t.Error("declared text target should not preserve images")
	}
	if ImageOKForTarget(cfg, nil, cat, configdomain.RouteTarget{Provider: "missing", Model: "text"}) {
		t.Error("catalog text target should not preserve images")
	}
	if !ImageOKForTarget(cfg, nil, cat, configdomain.RouteTarget{Provider: "missing", Model: "vision"}) {
		t.Error("catalog vision target should preserve images")
	}
	if !ImageOKForTarget(cfg, nil, cat, configdomain.RouteTarget{Provider: "missing", Model: "unknown"}) {
		t.Error("unknown target should preserve images rather than drop content")
	}
}

// TestConvertFixup_ImageOKForTarget preserves the protocol fixup routing
// contract under its historical top-level test name.
func TestConvertFixup_ImageOKForTarget(t *testing.T) {
	cat := catalog.New(map[string]catalog.Model{
		"m-vision": {Modalities: catalog.Modalities{Input: []string{"text", "image"}}},
		"m-text":   {Modalities: catalog.Modalities{Input: []string{"text"}}},
	})
	target := configdomain.RouteTarget{Provider: "p", Model: "m-vision"}
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"p": {},
	}}
	if !ImageOKForTarget(cfg, nil, cat, target) {
		t.Error("vision model via catalog must be image-ok")
	}
	if ImageOKForTarget(cfg, nil, cat, configdomain.RouteTarget{Provider: "p", Model: "m-text"}) {
		t.Error("text-only model via catalog must NOT be image-ok")
	}
	if !ImageOKForTarget(cfg, nil, cat, configdomain.RouteTarget{Provider: "p", Model: "m-unknown"}) {
		t.Error("unknown model must default to image-ok")
	}
	if !ImageOKForTarget(cfg, nil, nil, target) {
		t.Error("nil catalog must default to image-ok")
	}
	cfgWithCapabilities := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"p": {Capabilities: map[string][]string{
			"m-vision": {"text"},
			"m-text":   {"text", "image"},
		}},
	}}
	if ImageOKForTarget(cfgWithCapabilities, nil, cat, target) {
		t.Error("capabilities override (text-only) must beat catalog vision")
	}
	if !ImageOKForTarget(
		cfgWithCapabilities,
		nil,
		cat,
		configdomain.RouteTarget{Provider: "p", Model: "m-text"},
	) {
		t.Error("capabilities override (image) must beat catalog text-only")
	}
}
