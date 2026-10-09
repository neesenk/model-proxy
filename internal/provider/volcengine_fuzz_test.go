package provider

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

// FuzzParseVolcengineQuota pins the pure quota parser's structural contract
// on arbitrary (possibly malformed) upstream JSON: exactly four windows in
// fixed order, exactly one Ultimate (monthly) and one Short (5h), remaining
// percentages are finite and either -1 (unmeasured) or >= 0 — never NaN/Inf
// or negative (over-quota clamps to 0; a finite negative Used may legitimately
// exceed 1) — and the snapshot's scheduling base equals the Ultimate
// window's remaining.
func FuzzParseVolcengineQuota(f *testing.F) {
	seeds := []string{
		`{"plan_type":"agent","AFPFiveHour":{"Quota":100,"Used":20,"ResetTime":1750000000000},"AFPDaily":{"Quota":1000,"Used":900},"AFPWeekly":{"Quota":5000,"Used":100},"AFPMonthly":{"Quota":20000,"Used":15000}}`,
		`{"AFPMonthly":{"Quota":10,"Used":99}}`,
		`{"AFPFiveHour":{"Quota":0,"Used":0},"AFPMonthly":{"Quota":0,"Used":5}}`,
		`{"AFPMonthly":{"Quota":-5,"Used":-100}}`,
		`{}`,
		`{"AFPWeekly":{"Quota":1e308,"Used":-1e308}}`,
		`{"AFPMonthly":{"Quota":8,"Used":8}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var u AfpUsage
		if err := json.Unmarshal(raw, &u); err != nil {
			return
		}
		snap := ParseVolcengineQuota(&u)
		if len(snap.Windows) != 4 {
			t.Fatalf("windows = %d, want 4", len(snap.Windows))
		}
		want := []struct {
			label    string
			ultimate bool
			short    bool
		}{{"5h", false, true}, {"daily", false, false}, {"weekly", false, false}, {"monthly", true, false}}
		for i, w := range snap.Windows {
			if w.Label != want[i].label || w.Ultimate != want[i].ultimate || w.Short != want[i].short {
				t.Fatalf("window %d = %+v, want %s ultimate=%v short=%v", i, w, want[i].label, want[i].ultimate, want[i].short)
			}
			if math.IsNaN(w.RemainingPct) || math.IsInf(w.RemainingPct, 0) {
				t.Fatalf("window %s remaining = %v, want a finite value", w.Label, w.RemainingPct)
			}
			if w.RemainingPct != -1 && w.RemainingPct < 0 {
				t.Fatalf("window %s remaining = %v, want -1 (unmeasured) or >= 0", w.Label, w.RemainingPct)
			}
		}
		if snap.RemainingPct != snap.Windows[3].RemainingPct {
			t.Fatalf("snapshot remaining %v != monthly %v", snap.RemainingPct, snap.Windows[3].RemainingPct)
		}
		again := ParseVolcengineQuota(&u)
		if !reflect.DeepEqual(snap.Windows, again.Windows) {
			t.Fatal("windows differ between parses")
		}
	})
}
