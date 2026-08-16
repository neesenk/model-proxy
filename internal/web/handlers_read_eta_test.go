package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/appapi"
	"model-proxy/internal/provider"
)

// TestReadStatusQuotaExhaustionEta: /api/status serializes quota snapshots
// verbatim (PascalCase), so the tracker-computed QuotaSnapshot.ExhaustionEta
// must reach the Web UI as "ExhaustionEta" — the quota card's burn-rate
// prediction ("exhausts in ~40m at current rate") reads exactly this field.
func TestReadStatusQuotaExhaustionEta(t *testing.T) {
	eta := time.Date(2026, 8, 16, 12, 40, 0, 0, time.UTC)
	reads := &readAPIStub{dashboard: appapi.Dashboard{
		Quota: map[string]any{"p": &provider.QuotaSnapshot{
			Billing: provider.BillingPlan, RemainingPct: 0.2,
			Windows: []provider.QuotaWindow{{
				Label: "Weekly tokens", Kind: "tokens", Used: 800, Total: 1000,
				RemainingPct: 0.2, Ultimate: true,
			}},
			ExhaustionEta: eta,
		}},
	}}
	s := newReadServer(t, reads)
	resp := serveRead(t, s, http.MethodGet, "/api/status")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Code)
	}
	if !strings.Contains(resp.Body.String(), `"ExhaustionEta":"2026-08-16T12:40:00Z"`) {
		t.Errorf("quota snapshot lost ExhaustionEta in /api/status:\n%s", resp.Body.String())
	}
}
