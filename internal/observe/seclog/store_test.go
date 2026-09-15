package seclog

import (
	"testing"
)

// Counts aggregates verdict totals over the query predicate (kind/from/to),
// independent of the page limit — the KPI tiles' durable source.
func TestStoreCountsAggregation(t *testing.T) {
	dir := t.TempDir()
	seed := []Record{
		{Ts: 1000, Kind: "secret", Names: []string{"k"}, Action: "log", Verdict: "high"},
		{Ts: 2000, Kind: "secret", Names: []string{"k"}, Action: "log", Verdict: "high"},
		{Ts: 3000, Kind: "path", Names: []string{"ssh"}, Action: "log", Verdict: "medium"},
		{Ts: 4000, Kind: "secret", Names: []string{"k"}, Action: "log", Verdict: "error"},
		{Ts: 5000, Kind: "secret", Names: []string{"k"}, Action: "log"},
	}
	for i := range seed {
		if err := AppendSync(dir, &seed[i]); err != nil {
			t.Fatal(err)
		}
	}
	all, err := Counts(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if all["high"] != 2 || all["medium"] != 1 || all["error"] != 1 {
		t.Errorf("counts all = %v", all)
	}
	if all["low"] != 0 {
		t.Errorf("low must never be in the store: %v", all)
	}
	// Window: from cuts the two highs; kind narrows to the path medium.
	narrow, err := Counts(dir, Filter{From: 3500})
	if err != nil {
		t.Fatal(err)
	}
	if narrow["high"] != 0 || narrow["error"] != 1 {
		t.Errorf("counts windowed = %v", narrow)
	}
	pathOnly, err := Counts(dir, Filter{Kind: "path"})
	if err != nil {
		t.Fatal(err)
	}
	if pathOnly["medium"] != 1 || len(pathOnly) != 1 {
		t.Errorf("counts kind=path = %v", pathOnly)
	}
	// Missing store: empty map, no file created (query never creates).
	empty, err := Counts(t.TempDir(), Filter{})
	if err != nil || len(empty) != 0 {
		t.Errorf("missing store counts = %v err=%v", empty, err)
	}
}

// QueryByRequestIDs is the request↔guard correlation source: one batched
// lookup mapping request ids onto their audit rows, newest first per id,
// low verdicts excluded by the store as everywhere else.
func TestStoreQueryByRequestIDs(t *testing.T) {
	dir := t.TempDir()
	seed := []Record{
		{Ts: 1000, Kind: "secret", RequestID: "r1", Names: []string{"jwt"}, Action: "block"},
		{Ts: 2000, Kind: "secret", RequestID: "r1", Names: []string{"jwt"}, Action: "log", Verdict: "high", Reason: "real"},
		{Ts: 3000, Kind: "path", RequestID: "r2", Names: []string{"ssh"}, Action: "log", Verdict: "medium"},
		{Ts: 4000, Kind: "secret", RequestID: "r1", Names: []string{"jwt"}, Action: "log", Verdict: "low"},
		{Ts: 5000, Kind: "unblock", RequestID: "r1", SessionID: "s", Names: []string{"jwt"}, Action: "unblock"},
		{Ts: 6000, Kind: "secret", Names: []string{"jwt"}, Action: "log"}, // no request id
	}
	for i := range seed {
		if err := AppendSync(dir, &seed[i]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := QueryByRequestIDs(dir, []string{"r1", "r2", "r1", "", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ids = %v, want exactly r1 and r2", keysOf(got))
	}
	// r1: newest first — unblock(5000), high(2000), classic block(1000); the
	// low verdict (4000) never entered the store.
	r1 := got["r1"]
	if len(r1) != 3 || r1[0].Kind != "unblock" || r1[1].Verdict != "high" || r1[2].Action != "block" {
		t.Errorf("r1 rows = %+v", r1)
	}
	if len(got["r2"]) != 1 || got["r2"][0].Verdict != "medium" {
		t.Errorf("r2 rows = %+v", got["r2"])
	}
	// Empty batch and a store-less directory both stay empty without error.
	if m, err := QueryByRequestIDs(dir, nil); err != nil || len(m) != 0 {
		t.Errorf("nil ids = %v err=%v", m, err)
	}
	if m, err := QueryByRequestIDs(t.TempDir(), []string{"r1"}); err != nil || len(m) != 0 {
		t.Errorf("missing store = %v err=%v", m, err)
	}
}

func keysOf(m map[string][]*Record) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
