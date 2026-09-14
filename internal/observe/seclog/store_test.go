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
