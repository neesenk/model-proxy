package wirecap

import (
	"encoding/json"
	"errors"
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
	caps := func(responses, anthropic Verdict) Capabilities {
		return Capabilities{Responses: responses, Anthropic: anthropic}
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
		{"responses verdict ignored without store hit", "responses", false, caps(No, Yes), false, "responses", false},
		{"responses yes", "responses", false, caps(Yes, Unknown), true, "responses", false},
		{"responses no", "responses", false, caps(No, No), true, "openai", false},
		{"anthropic base", "anthropic", true, caps(Yes, No), true, "anthropic", false},
		{"anthropic accepted", "anthropic", false, caps(Unknown, Yes), true, "anthropic", false},
		{"anthropic accepted before responses fallback", "anthropic", false, caps(No, Yes), true, "anthropic", false},
		{"anthropic to responses", "anthropic", false, caps(Yes, Unknown), true, "responses", true},
		{"anthropic to chat", "anthropic", false, caps(No, Unknown), true, "openai", false},
		{"anthropic negative legs", "anthropic", false, caps(No, No), true, "openai", false},
		{"anthropic unknown", "anthropic", false, caps(Unknown, Unknown), true, "anthropic", false},
		{"anthropic explicit no without responses verdict", "anthropic", false, caps(Unknown, No), true, "anthropic", false},
		{"anthropic verdicts ignored without store hit", "anthropic", false, caps(Yes, Yes), false, "anthropic", false},
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

func TestStoreSnapshotCorrectionAndRestore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := NewStore()
	store.Put("a", Capabilities{
		BaseURL: "https://a", Responses: Yes, Anthropic: No, ProbedAt: now,
	})
	store.Put("b", Capabilities{
		BaseURL: "https://old-b", Responses: No, Anthropic: Yes, ProbedAt: now,
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
			BaseURL: "https://a", Responses: No, Anthropic: No,
			ProbedAt: now.Add(time.Minute),
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
	store := NewStore()
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
