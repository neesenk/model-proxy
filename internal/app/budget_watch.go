package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	observeevents "model-proxy/internal/observe/events"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

// budgetAlertPayload is the JSON body POSTed to budgets.webhook_url and the
// serialized Detail of the "budget" live event.
type budgetAlertPayload struct {
	Scope        string  `json:"scope"` // "global" or a provider name
	Month        string  `json:"month"` // local calendar month, e.g. "2026-08"
	ThresholdUSD float64 `json:"threshold_usd"`
	ActualUSD    float64 `json:"actual_usd"`
}

// budgetWatcher is the Proxy-owned monthly equivalent-cost alert loop. Once
// per minute it reads the current local month's accumulated cost from the
// stats store through the same pricing path as /api/analytics and fires an
// alert (live event + optional webhook) once per (scope, month, threshold)
// per process. It owns no durable state, so there is nothing to flush on
// shutdown; the loop is stopped and waited by the Proxy lifecycle like the
// stats flusher.
type budgetWatcher struct {
	proxy        *Proxy
	client       *http.Client
	now          func() time.Time // clock seam for tests
	retryBackoff time.Duration    // webhook retry spacing (tests set 0)

	mu      sync.Mutex
	alerted map[string]bool
}

const (
	budgetWebhookTimeout = 5 * time.Second
	budgetWebhookRetries = 2 // retries after the first attempt (3 total)
)

// startBudgetWatcher starts the per-minute budget alert loop when any
// threshold is configured; it is a no-op otherwise, so an unconfigured proxy
// runs no background task at all. Threshold changes apply on reload (each
// check reads the current config snapshot), but enabling budgets from scratch
// requires a restart — same startup-only rule as request_log.
func (p *Proxy) startBudgetWatcher() {
	cfg := p.cfgSnapshot()
	if cfg == nil || !cfg.Budgets.Enabled() {
		return
	}
	watcher := &budgetWatcher{
		proxy:        p,
		client:       &http.Client{},
		now:          time.Now,
		retryBackoff: time.Second,
		alerted:      map[string]bool{},
	}
	p.budget = watcher
	p.lifecycle.Run(watcher.loop)
}

// loop checks once at startup, then right after each wall-clock minute
// boundary (aligned with the stats flush cadence so the freshest minute is
// already persisted).
func (w *budgetWatcher) loop(stop <-chan struct{}) {
	w.check(w.now())
	for {
		timer := time.NewTimer(observestats.UntilNextMinute(w.now()))
		select {
		case <-timer.C:
			w.check(w.now())
		case <-stop:
			timer.Stop()
			return
		}
	}
}

// check evaluates every configured budget scope against the current local
// month's accumulated equivalent cost and fires alerts for crossed
// thresholds. Query or pricing failures are logged and retried next tick.
func (w *budgetWatcher) check(now time.Time) {
	p := w.proxy
	// Capture reload-owned state once; the stats store and pricing carry
	// their own leaf locks, so nothing here nests under p.mu.
	p.mu.RLock()
	if p.cfg == nil {
		p.mu.RUnlock()
		return
	}
	budgets := p.cfg.Budgets
	parentOf := p.parentOf
	p.mu.RUnlock()
	if !budgets.Enabled() || p.stats == nil {
		return
	}

	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	month := monthStart.Format("2006-01")
	buckets, err := p.stats.QueryAnalytics(monthStart.Unix(), now.Unix(), "", "", "month")
	if err != nil {
		log.Printf("[budget] stats query failed: %v", err)
		return
	}
	// Same pricing snapshot as the analytics read view: config overrides
	// first, then the cached catalog (nil/disabled = only overrides price).
	pricingView := p.readView().pricing()

	total := 0.0
	perProvider := map[string]float64{}
	for _, bucket := range buckets {
		entry, ok := pricing.Resolve(pricingView.overrides, pricingView.catalog, bucket.Model)
		if !ok {
			continue // unpriced models contribute no known cost (n/a, as in /api/analytics)
		}
		cost := pricing.ComputeCost(bucket.Input, bucket.Output, bucket.CacheRead, bucket.CacheCreation, entry)
		total += cost
		name := bucket.Provider
		if parent := parentOf[name]; parent != "" {
			name = parent // attribute pooled virtual ids (name#account) to the parent
		}
		perProvider[name] += cost
	}

	if budgets.MonthlyUSD > 0 {
		w.maybeAlert(budgetAlertPayload{
			Scope: "global", Month: month,
			ThresholdUSD: budgets.MonthlyUSD, ActualUSD: total,
		}, budgets.WebhookURL, now)
	}
	for name, threshold := range budgets.Providers {
		if threshold <= 0 {
			continue
		}
		w.maybeAlert(budgetAlertPayload{
			Scope: name, Month: month,
			ThresholdUSD: threshold, ActualUSD: perProvider[name],
		}, budgets.WebhookURL, now)
	}
}

// maybeAlert fires the live event and the optional webhook exactly once per
// (scope, month, threshold) per process. Crossing below the threshold again
// (e.g. after stats reset) does not re-arm the alert within the process.
func (w *budgetWatcher) maybeAlert(payload budgetAlertPayload, webhookURL string, now time.Time) {
	if payload.ActualUSD < payload.ThresholdUSD {
		return
	}
	key := payload.Scope + "\x00" + payload.Month + "\x00" + strconv.FormatFloat(payload.ThresholdUSD, 'g', -1, 64)
	w.mu.Lock()
	if w.alerted[key] {
		w.mu.Unlock()
		return
	}
	w.alerted[key] = true
	w.mu.Unlock()

	detail, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[budget] marshal alert payload failed: %v", err)
		return
	}
	event := observeevents.Event{
		Type:   "budget",
		Ts:     now.UnixMilli(),
		Detail: string(detail),
	}
	if payload.Scope != "global" {
		event.Provider = payload.Scope
	}
	w.proxy.events.Publish(event)
	log.Printf("[budget] %s %s: actual $%.2f >= threshold $%.2f",
		payload.Scope, payload.Month, payload.ActualUSD, payload.ThresholdUSD)

	if webhookURL != "" {
		w.postWebhook(webhookURL, detail)
	}
}

// postWebhook delivers the alert payload with up to budgetWebhookRetries
// retries on transport errors and 5xx. Failures are logged only — alerting
// never disturbs the main path.
func (w *budgetWatcher) postWebhook(url string, body []byte) {
	for attempt := 0; attempt <= budgetWebhookRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(w.retryBackoff)
		}
		ctx, cancel := context.WithTimeout(context.Background(), budgetWebhookTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			log.Printf("[budget] webhook request build failed: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.client.Do(req)
		cancel()
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return // delivered, or permanently rejected (4xx) — do not retry
			}
		}
	}
	log.Printf("[budget] webhook POST %s failed after %d attempts", url, budgetWebhookRetries+1)
}
