package provider

import (
	"encoding/json"
	"model-proxy/internal/display"
	"os"
	"path/filepath"
	"time"
)

// quota_eta.go: exhaustion prediction ("at the current burn rate the ultimate
// window runs out in ~40m"). The estimate is DISPLAY-ONLY — scheduling never
// reads it. Two producers set QuotaSnapshot.ExhaustionEta:
//
//   - the daemon's quota tracker (internal/runtime), on every committed poll,
//     using the previously committed Manager snapshot as baseline;
//   - DecorateExhaustionEta, for the `usage` CLI — a separate process that
//     live-fetches a single snapshot, so its only baseline is the tracker's
//     last persisted snapshot in ~/.model-proxy/quota_state.json.

// DefaultEtaMaxGap bounds the snapshot gap the CLI decoration accepts: the
// default poll interval is 5m, so 3× that = 15m. A wider gap means the daemon
// stopped polling (or was down) and the persisted baseline is stale — no
// prediction. The tracker itself uses 3× the CONFIGURED poll interval.
const DefaultEtaMaxGap = 15 * time.Minute

// EstimateExhaustionEta predicts when cur's ultimate (total-budget) window will
// be exhausted, from the burn rate between prev and cur:
//
//	rate      = (cur.used − prev.used) / (cur.as_of − prev.as_of)
//	eta       = cur.as_of + (cur.total − cur.used) / rate
//
// Returns the zero time (no prediction) when: either side is missing or carries
// an error, the ultimate window is absent/unmeasured, the gap is non-positive
// or exceeds maxGap (poll gap → stale baseline), usage did not increase
// (rate ≤ 0 — idle, or a window reset between snapshots), or the window is
// already exhausted.
func EstimateExhaustionEta(prev, cur *QuotaSnapshot, maxGap time.Duration) time.Time {
	if prev == nil || cur == nil || prev.Err != "" || cur.Err != "" {
		return time.Time{}
	}
	if prev.AsOf.IsZero() || cur.AsOf.IsZero() {
		return time.Time{}
	}
	dt := cur.AsOf.Sub(prev.AsOf)
	if dt <= 0 || (maxGap > 0 && dt > maxGap) {
		return time.Time{}
	}
	pw, cw := ultimateWindow(prev), ultimateWindow(cur)
	if pw == nil || cw == nil || pw.Total <= 0 || cw.Total <= 0 || cw.RemainingPct < 0 {
		return time.Time{}
	}
	rate := (cw.Used - pw.Used) / dt.Seconds()
	if rate <= 0 {
		return time.Time{}
	}
	remaining := cw.Total - cw.Used
	if remaining <= 0 {
		return time.Time{}
	}
	return cur.AsOf.Add(time.Duration(remaining / rate * float64(time.Second)))
}

// ultimateWindow returns the snapshot's total-budget window (nil when absent).
func ultimateWindow(s *QuotaSnapshot) *QuotaWindow {
	for i := range s.Windows {
		if s.Windows[i].Ultimate {
			return &s.Windows[i]
		}
	}
	return nil
}

// exhaustionHint renders the CLI window-line suffix "· 按当前速率 ~40m 后耗尽"
// (Gray; "" when there is no prediction or it is already in the past).
func exhaustionHint(eta, now time.Time) string {
	secs := int(eta.Sub(now) / time.Second)
	if secs <= 0 {
		return ""
	}
	return display.Gray(" · 按当前速率 ~" + display.FormatDuration(secs) + " 后耗尽")
}

// DecorateExhaustionEta sets s.ExhaustionEta for the `usage` CLI: the CLI
// live-fetches one snapshot, so the burn-rate baseline is the daemon tracker's
// last persisted snapshot (~/.model-proxy/quota_state.json). Best-effort — no
// daemon/no file/stale file/pooled virtual keys all yield no prediction, never
// an error. Keyed by plain provider name; pooled accounts (state keys are
// "name#accountID" virtuals) get no CLI prediction.
func DecorateExhaustionEta(providerName string, s *QuotaSnapshot) {
	if s == nil || s.Err != "" {
		return
	}
	if s.AsOf.IsZero() {
		s.AsOf = time.Now()
	}
	s.ExhaustionEta = EstimateExhaustionEta(loadPersistedQuotaBaseline(providerName), s, DefaultEtaMaxGap)
}

// loadPersistedQuotaBaseline reads the daemon-persisted snapshot for
// providerName from ~/.model-proxy/quota_state.json. Read-only projection of
// the format internal/runtime.PersistedQuotaSnapshot writes (the write owner —
// provider cannot import runtime, that would be an import cycle); only the
// fields EstimateExhaustionEta needs are decoded. Returns nil on any failure.
func loadPersistedQuotaBaseline(providerName string) *QuotaSnapshot {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".model-proxy", "quota_state.json"))
	if err != nil {
		return nil
	}
	var wrap struct {
		Providers map[string]struct {
			Windows []QuotaWindow `json:"windows"`
			AsOf    time.Time     `json:"as_of"`
			Err     string        `json:"err"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil
	}
	p, ok := wrap.Providers[providerName]
	if !ok {
		return nil
	}
	return &QuotaSnapshot{Windows: p.Windows, AsOf: p.AsOf, Err: p.Err}
}
