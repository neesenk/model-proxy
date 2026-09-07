package wirecap

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestVerdictStringJSONAndParse(t *testing.T) {
	cases := []struct {
		verdict Verdict
		text    string
	}{
		{Unknown, "unknown"},
		{Yes, "yes"},
		{No, "no"},
	}
	for _, testCase := range cases {
		if got := testCase.verdict.String(); got != testCase.text {
			t.Errorf("%d String = %q, want %q", testCase.verdict, got, testCase.text)
		}
		if got := ParseVerdict(testCase.text); got != testCase.verdict {
			t.Errorf("ParseVerdict(%q) = %d, want %d", testCase.text, got, testCase.verdict)
		}
		data, err := json.Marshal(testCase.verdict)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Verdict
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded != testCase.verdict {
			t.Errorf("JSON round trip = %d, want %d", decoded, testCase.verdict)
		}
	}
	if got := ParseVerdict("invalid"); got != Unknown {
		t.Errorf("invalid verdict = %d, want Unknown", got)
	}
}

func TestLegFresh(t *testing.T) {
	now := time.Unix(10_000, 0)
	const ttl = 24 * time.Hour
	if !LegFresh(Yes, time.Time{}, now, ttl) {
		t.Error("positive verdict must stay fresh")
	}
	if !LegFresh(No, now.Add(-ttl+time.Second), now, ttl) {
		t.Error("recent negative verdict must be fresh")
	}
	if LegFresh(No, now.Add(-ttl), now, ttl) {
		t.Error("negative verdict at TTL boundary must be stale")
	}
	if LegFresh(Unknown, now, now, ttl) {
		t.Error("unknown verdict must not be fresh")
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   Verdict
	}{
		{404, nil, No},
		{200, nil, Yes},
		{201, nil, Yes},
		{400, nil, Yes},
		{401, nil, Yes},
		{403, nil, Yes},
		{429, nil, Yes},
		{500, nil, Unknown},
		{502, nil, Unknown},
		{0, errors.New("transport"), Unknown},
	}
	for _, testCase := range cases {
		if got := ClassifyStatus(testCase.status, testCase.err); got != testCase.want {
			t.Errorf(
				"ClassifyStatus(%d, %v) = %s, want %s",
				testCase.status,
				testCase.err,
				got,
				testCase.want,
			)
		}
	}
}

func TestResolve(t *testing.T) {
	caps := func(chat, responses Verdict) Capabilities {
		return Capabilities{Chat: chat, Responses: responses}
	}
	cases := []struct {
		name               string
		clientProtocol     string
		hasAnthropicBase   bool
		capabilities       Capabilities
		ok                 bool
		wantProtocol       string
		wantResponsesRoute bool
	}{
		{"chat passthrough", "openai", false, caps(No, No), true, "openai", false},
		{"responses unknown", "responses", false, caps(Unknown, Unknown), false, "responses", false},
		{"responses verdict ignored without store hit", "responses", false, caps(Yes, No), false, "responses", false},
		{"responses yes", "responses", false, caps(Unknown, Yes), true, "responses", false},
		{"responses no", "responses", false, caps(No, No), true, "openai", false},
		{"anthropic base", "anthropic", true, caps(No, Yes), true, "anthropic", false},
		{"anthropic to responses", "anthropic", false, caps(Unknown, Yes), true, "responses", true},
		{"anthropic to chat", "anthropic", false, caps(Unknown, No), true, "openai", false},
		{"anthropic negative legs", "anthropic", false, caps(No, No), true, "openai", false},
		{"anthropic unknown", "anthropic", false, caps(Unknown, Unknown), true, "anthropic", false},
		{"anthropic verdicts ignored without store hit", "anthropic", false, caps(Yes, Yes), false, "anthropic", false},
		// No anthropic base means anthropic is unsupported by definition: no
		// passthrough of anthropic bodies onto the openai base, even when the
		// chat leg concluded yes (the gateway might have accepted it — the
		// proxy now always converts instead).
		{"anthropic never passthrough on openai base", "anthropic", false, caps(Yes, No), true, "openai", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			protocol, via := Resolve(
				testCase.clientProtocol,
				testCase.hasAnthropicBase,
				testCase.capabilities,
				testCase.ok,
			)
			if protocol != testCase.wantProtocol || via != testCase.wantResponsesRoute {
				t.Errorf(
					"Resolve = (%q, %v), want (%q, %v)",
					protocol,
					via,
					testCase.wantProtocol,
					testCase.wantResponsesRoute,
				)
			}
		})
	}
}

func TestClassifyModelStatus(t *testing.T) {
	cases := []struct {
		name   string
		probed bool
		status int
		err    error
		body   string
		want   Verdict
	}{
		{"unprobed leg is definitionally no", false, 0, nil, "", No},
		{"transport error", true, 0, errors.New("dial"), "", Unknown},
		{"2xx", true, 200, nil, "", Yes},
		{"404", true, 404, nil, "", No},
		{"400 shape dispute", true, 400, nil, `{"error":{"message":"Unsupported parameter: 'stream'"}}`, Yes},
		{"400 model not found", true, 400, nil, `{"error":{"message":"model not found: m1"}}`, No},
		{"400 does not exist", true, 400, nil, `The model 'm1' does not exist`, No},
		{"400 unsupported model", true, 400, nil, `unsupported model`, No},
		{"400 invalid model", true, 400, nil, `invalid model`, No},
		{"400 not supported with this model", true, 400, nil, `Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.`, No},
		{"400 case insensitive", true, 400, nil, `MODEL NOT FOUND`, No},
		// v3 additions — shopee's retcode 40403 envelope, both spellings.
		{"400 retcode model not supported", true, 400, nil, `{"retcode":40403,"message":"Model not supported by this endpoint"}`, No},
		{"400 model not supported without is", true, 400, nil, `Model not supported on this plan`, No},
		// Reverse: "supported" substrings that are NOT model rejections must
		// stay shape-dispute yes.
		{"400 streaming not supported is not model denial", true, 400, nil, `Streaming is not supported for this plan tier`, Yes},
		{"400 parameter not supported is shape dispute", true, 400, nil, `Unsupported parameter: 'temperature' is not supported`, Yes},
		{"400 tools not supported for model on leg", true, 400, nil, `Function tools with reasoning_effort are not supported for gpt-5.6-luna in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.`, No},
		{"401 auth", true, 401, nil, "", Unknown},
		{"429 quota", true, 429, nil, "", Unknown},
		{"500", true, 500, nil, "", Unknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ClassifyModelStatus(testCase.probed, testCase.status, testCase.err, []byte(testCase.body))
			if got != testCase.want {
				t.Errorf("ClassifyModelStatus = %s, want %s", got, testCase.want)
			}
		})
	}
}

func TestResolveModel(t *testing.T) {
	mp := func(chat, anthropic, responses Verdict) ModelProtocols {
		return ModelProtocols{Chat: chat, Anthropic: anthropic, Responses: responses}
	}
	providerCaps := Capabilities{Chat: Unknown, Responses: Yes}
	cases := []struct {
		name             string
		clientProtocol   string
		hasAnthropicBase bool
		modelCaps        ModelProtocols
		mok              bool
		wantProtocol     string
		wantVia          bool
	}{
		// Model-level miss → provider-level Resolve (responses=yes here).
		{"miss falls back to provider", "anthropic", false, ModelProtocols{}, false, "responses", true},
		// anthropic client.
		{"anthropic yes with base", "anthropic", true, mp(Yes, Yes, No), true, "anthropic", false},
		{"anthropic no with base, provider responses yes", "anthropic", true, mp(Yes, No, Unknown), true, "responses", true},
		{"model absent from anthropic base, responses yes", "anthropic", true, mp(Yes, No, Yes), true, "responses", true},
		{"no anthropic base, responses yes", "anthropic", false, mp(Yes, No, Yes), true, "responses", true},
		{"no anthropic base, responses no, chat yes", "anthropic", false, mp(Yes, No, No), true, "openai", false},
		{"all no falls to chat", "anthropic", false, mp(No, No, No), true, "openai", false},
		{"unknown legs fall back to provider", "anthropic", false, mp(Unknown, No, Unknown), true, "responses", true},
		{"anthropic yes but no base must not passthrough", "anthropic", false, mp(Yes, Yes, No), true, "openai", false},
		// responses client.
		{"responses client yes", "responses", false, mp(Yes, No, Yes), true, "responses", false},
		{"responses client no", "responses", false, mp(Yes, No, No), true, "openai", false},
		{"responses client unknown falls back", "responses", false, mp(Unknown, No, Unknown), true, "responses", false},
		// unknown alternative beats a known-dead leg (low-11 contract)
		{"responses dead tries unknown anthropic", "responses", false, mp(No, Unknown, No), true, "anthropic", false},
		{"responses dead all-no stays chat", "responses", false, mp(No, No, No), true, "openai", false},
		{"chat dead tries unknown responses", "openai", false, mp(No, Unknown, Unknown), true, "responses", true},
		{"chat dead all-no stays passthrough", "openai", false, mp(No, No, No), true, "openai", false},
		// chat client: a probed chat no converts to the first probed-yes leg
		// instead of passthroughing the dead chat leg.
		{"chat client converts off dead chat leg", "openai", false, mp(No, No, Yes), true, "responses", true},
		{"chat client converts to anthropic when only leg", "openai", true, mp(No, Yes, No), true, "anthropic", false},
		{"chat client unconcluded stays passthrough", "openai", false, mp(Unknown, No, Unknown), true, "openai", false},
		{"chat client all-no stays passthrough", "openai", false, mp(No, No, No), true, "openai", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			protocol, via := ResolveModel(
				testCase.clientProtocol,
				testCase.hasAnthropicBase,
				testCase.modelCaps,
				testCase.mok,
				providerCaps,
				true,
			)
			if protocol != testCase.wantProtocol || via != testCase.wantVia {
				t.Errorf("ResolveModel = (%q, %v), want (%q, %v)", protocol, via, testCase.wantProtocol, testCase.wantVia)
			}
		})
	}
}

func TestStoreSnapshotCorrectionAndRestore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &Store{}
	store.Put("a", Capabilities{
		BaseURL: "https://a", Responses: Yes, Chat: No, ProbedAt: now, ProbeVersion: ProbeVersion,
	})
	store.Put("b", Capabilities{
		BaseURL: "https://old-b", Responses: No, Chat: Yes, ProbedAt: now, ProbeVersion: ProbeVersion,
	})
	store.MarkResponsesUnsupported("a", now.Add(time.Minute))
	got, ok := store.Get("a")
	if !ok || got.Responses != No || !got.ProbedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("corrected capabilities = %+v, ok=%v", got, ok)
	}

	snapshot := store.Snapshot()
	snapshot["a"] = Capabilities{}
	if detached, _ := store.Get("a"); detached.BaseURL != "https://a" {
		t.Fatal("Snapshot returned a map alias")
	}

	store.RestoreMatching(store.Snapshot(), map[string]string{
		"a": "https://a",
		"b": "https://new-b",
	})
	want := map[string]Capabilities{
		"a": {
			BaseURL: "https://a", Responses: No, Chat: No,
			ProbedAt: now.Add(time.Minute), ProbeVersion: ProbeVersion,
		},
	}
	if restored := store.Snapshot(); !reflect.DeepEqual(restored, want) {
		t.Errorf("restored = %+v, want %+v", restored, want)
	}

	store.RestoreMatching(
		map[string]Capabilities{"ghost": {BaseURL: ""}},
		map[string]string{},
	)
	if restored := store.Snapshot(); len(restored) != 0 {
		t.Errorf("unknown provider with empty base URL was restored: %+v", restored)
	}

	var nilStore *Store
	if _, ok := nilStore.Get("a"); ok {
		t.Error("nil Store Get succeeded")
	}
	if got := nilStore.Snapshot(); len(got) != 0 {
		t.Errorf("nil Store Snapshot = %+v", got)
	}
	nilStore.Put("a", Capabilities{})
	nilStore.MarkResponsesUnsupported("a", now)
	nilStore.RestoreMatching(nil, nil)
}

func TestStoreConcurrentAccess(t *testing.T) {
	store := &Store{}
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			for iteration := 0; iteration < 100; iteration++ {
				store.Put("provider", Capabilities{Responses: Yes})
				store.MarkResponsesUnsupported("provider", time.Unix(int64(iteration), 0))
				store.Get("provider")
				store.Snapshot()
			}
			done <- struct{}{}
		}()
	}
	close(start)
	<-done
	<-done

	if _, ok := store.Get("provider"); !ok {
		t.Fatal("concurrent writers lost the provider entry")
	}
}

// TestResolveModelAnthropicNoNeverPassthroughs pins the layer-agreement fix:
// a concluded model-level anthropic no must not fall through to provider-level
// anthropic passthrough even when the provider-level legs are unconcluded
// (routing.NativeProtocolsWithVerdict excludes the leg, so forward must too).
func TestResolveModelAnthropicNoNeverPassthroughs(t *testing.T) {
	caps := Capabilities{Chat: Unknown, Responses: Unknown}
	for _, hasBase := range []bool{true, false} {
		proto, via := ResolveModel("anthropic", hasBase,
			ModelProtocols{Chat: Unknown, Anthropic: No, Responses: Unknown}, true, caps, true)
		if proto == "anthropic" {
			t.Fatalf("hasBase=%v: ResolveModel passthroughs dead anthropic leg (%q, via=%v)", hasBase, proto, via)
		}
	}
}

// TestClassifyProviderStatus pins the provider-level agent-grade 400 rule:
// tool/model rejection wording → No, any other 400 keeps the generic
// "shape dispute proves the route exists" yes.
func TestClassifyProviderStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
		body   string
		want   Verdict
	}{
		{"200", 200, nil, "", Yes},
		{"404", 404, nil, "", No},
		{"plain 400 shape dispute", 400, nil, `Unsupported parameter: 'temperature'`, Yes},
		{"400 tool rejection wording", 400, nil, `Function tools with reasoning_effort are not supported for gpt-5.6-luna in /v1/chat/completions`, No},
		{"400 model not found", 400, nil, `Model not found: m1`, No},
		// v3 additions — the shopee envelope that used to read as a
		// shape-dispute yes (the false-positive that motivated v3).
		{"400 retcode not supported by this endpoint", 400, nil, `{"retcode":40403,"message":"Model not supported by this endpoint"}`, No},
		{"400 streaming not supported stays yes", 400, nil, `Streaming is not supported for this plan tier`, Yes},
		{"401", 401, nil, "", Yes},
		{"500", 500, nil, "", Unknown},
		{"network error", 0, fmt.Errorf("dial"), "", Unknown},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ClassifyProviderStatus(testCase.status, testCase.err, []byte(testCase.body))
			if got != testCase.want {
				t.Errorf("ClassifyProviderStatus = %s, want %s", got, testCase.want)
			}
		})
	}
}
