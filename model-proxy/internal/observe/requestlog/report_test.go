package requestlog

import (
	"reflect"
	"testing"
)

func TestShadowReportAggregatesMultipleGroupsWithAllFields(t *testing.T) {
	dir := t.TempDir()
	primaries := []Record{
		{Ts: "2026-07-19T01:00:00Z", RequestID: "b1", Exposed: "beta", Provider: "primary-b", Status: 200, LatencyMs: 10, ResponseSize: 1},
		{Ts: "2026-07-19T01:00:01Z", RequestID: "b2", Exposed: "beta", Provider: "primary-b", Status: 500, LatencyMs: 20, ResponseSize: 2},
		{Ts: "2026-07-19T01:00:02Z", RequestID: "b3", Exposed: "beta", Provider: "primary-b", Status: 200, LatencyMs: 30, ResponseSize: 3},
		{Ts: "2026-07-19T01:00:03Z", RequestID: "a1", Exposed: "alpha", Provider: "primary-a", Status: 200, LatencyMs: 100, ResponseSize: 1000},
		{Ts: "2026-07-19T01:00:04Z", RequestID: "a2", Exposed: "alpha", Provider: "primary-a", Status: 500, LatencyMs: 200, ResponseSize: 1400},
		{Ts: "2026-07-19T01:00:05Z", RequestID: "g1", Exposed: "gamma", Provider: "primary-g", Status: 429, LatencyMs: 80, ResponseSize: 11},
		{Ts: "2026-07-19T01:00:06Z", RequestID: "lonely-primary", Exposed: "ignored", Provider: "primary", Status: 200},
	}
	shadows := []Record{
		{Ts: "2026-07-19T01:01:00Z", RequestID: "shadow-b1", Provider: "shadow-b", Status: 201, LatencyMs: 40, ResponseSize: 4, Shadow: true},
		{Ts: "2026-07-19T01:01:01Z", RequestID: "shadow-b2", Provider: "shadow-b", Status: 503, LatencyMs: 50, ResponseSize: 5, Shadow: true},
		{Ts: "2026-07-19T01:01:02Z", RequestID: "shadow-b3", Provider: "shadow-b", Status: 500, LatencyMs: 60, ResponseSize: 6, Shadow: true},
		{Ts: "2026-07-19T01:01:03Z", RequestID: "shadow-a1", Provider: "shadow-a", Status: 200, LatencyMs: 160, ResponseSize: 800, Shadow: true},
		{Ts: "2026-07-19T01:01:04Z", RequestID: "shadow-a2", Provider: "shadow-a", Status: 200, LatencyMs: 260, ResponseSize: 1000, Shadow: true},
		{Ts: "2026-07-19T01:01:05Z", RequestID: "shadow-g1", Provider: "shadow-g", Status: 500, LatencyMs: 20, ResponseSize: 33, Shadow: true},
		{Ts: "2026-07-19T01:01:06Z", RequestID: "shadow-lonely-shadow", Provider: "shadow", Status: 200, Shadow: true},
	}
	writeRecordFile(t, dir, "requests-20260719-010000.log", primaries)
	writeRecordFile(t, dir, "requests-20260719-010100.log", shadows)

	got, err := ShadowReport(dir, Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []ShadowReportEntry{
		{
			Route: "beta", PrimaryProvider: "primary-b", ShadowProvider: "shadow-b",
			Samples: 3, StatusMatchRate: 2.0 / 3.0,
			PrimaryLatencyMs: 20, ShadowLatencyMs: 50, LatencyDiffMs: 30,
			PrimarySizeAvg: 2, ShadowSizeAvg: 5,
		},
		{
			Route: "alpha", PrimaryProvider: "primary-a", ShadowProvider: "shadow-a",
			Samples: 2, StatusMatchRate: 0.5,
			PrimaryLatencyMs: 150, ShadowLatencyMs: 210, LatencyDiffMs: 60,
			PrimarySizeAvg: 1200, ShadowSizeAvg: 900,
		},
		{
			Route: "gamma", PrimaryProvider: "primary-g", ShadowProvider: "shadow-g",
			Samples: 1, StatusMatchRate: 1,
			PrimaryLatencyMs: 80, ShadowLatencyMs: 20, LatencyDiffMs: -60,
			PrimarySizeAvg: 11, ShadowSizeAvg: 33,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ShadowReport =\n%+v\nwant\n%+v", got, want)
	}
}

func TestShadowReportSkipsUnpairedRecords(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(t, dir, "requests-20260719-010000.log", []Record{
		{Ts: "2026-07-19T01:00:00Z", RequestID: "paired", Exposed: "glm", Provider: "p", Status: 200},
		{Ts: "2026-07-19T01:00:01Z", RequestID: "shadow-paired", Provider: "s", Status: 500, Shadow: true},
		{Ts: "2026-07-19T01:00:02Z", RequestID: "primary-only", Exposed: "glm", Provider: "p", Status: 200},
		{Ts: "2026-07-19T01:00:03Z", RequestID: "shadow-shadow-only", Provider: "s", Status: 200, Shadow: true},
	})
	entries, err := ShadowReport(dir, Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Samples != 1 {
		t.Errorf("entries = %+v, want only the paired sample", entries)
	}
}
