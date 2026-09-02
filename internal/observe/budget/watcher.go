// Package budget owns the monthly equivalent-cost alert loop: per-minute
// wall-clock ticks (aligned with the stats flush cadence), once-per-process
// dedup per (scope, month, threshold), live-event + optional webhook alerting
// with bounded retries, and stop-abort so an unresponsive endpoint cannot pin
// the lifecycle wait. The watcher owns no durable state, so there is nothing
// to flush on shutdown. All reload-owned inputs reach the package through
// narrow copy-by-value ports supplied by the composition root.
package budget

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	configdomain "model-proxy/internal/config"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/logx"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

// Ports are the narrow per-tick inputs the watcher needs from the composition
// root. BudgetState and PricingSnapshot must return per-tick COPIES — never
// live map references into reload-owned state (the providers/parentOf maps
// must be freshly copied on every call).
type Ports struct {
	BudgetState     func() (budgets configdomain.BudgetsConfig, parentOf map[string]string, ok bool)
	QueryAnalytics  func(from, to int64) ([]observestats.AnalyticsBucket, error)
	PricingSnapshot func() (overrides map[string]pricing.Override, catalog *pricing.Catalog)
	Publish         func(observeevents.Event)
}

// alertPayload is the JSON body POSTed to budgets.webhook_url and the
// serialized Detail of the "budget" live event.
type alertPayload struct {
	Scope        string  `json:"scope"` // "global" or a provider name
	Month        string  `json:"month"` // local calendar month, e.g. "2026-08"
	ThresholdUSD float64 `json:"threshold_usd"`
	ActualUSD    float64 `json:"actual_usd"`
}

// Watcher is the monthly equivalent-cost alert loop. Once per minute it reads
// the current local month's accumulated cost from the stats store through the
// same pricing path as /api/analytics and fires an alert (live event +
// optional webhook) once per (scope, month, threshold) per process.
type Watcher struct {
	ports        Ports
	client       *http.Client
	now          func() time.Time // clock seam for tests
	retryBackoff time.Duration    // webhook retry spacing (tests set 0)

	mu      sync.Mutex
	alerted map[string]bool
}

const (
	webhookTimeout = 5 * time.Second
	webhookRetries = 2 // retries after the first attempt (3 total)
)

// NewWatcher builds the alert loop around the given ports. A nil client gets
// the zero http.Client (no timeout of its own — each attempt carries
// webhookTimeout via request context).
func NewWatcher(ports Ports, client *http.Client) *Watcher {
	if client == nil {
		client = &http.Client{}
	}
	return &Watcher{
		ports:        ports,
		client:       client,
		now:          time.Now,
		retryBackoff: time.Second,
		alerted:      map[string]bool{},
	}
}

// Loop checks once at startup, then right after each wall-clock minute
// boundary (aligned with the stats flush cadence so the freshest minute is
// already persisted). stop also aborts in-flight webhook retries so an
// unresponsive endpoint cannot pin Proxy.Close.
func (w *Watcher) Loop(stop <-chan struct{}) {
	w.check(w.now(), stop)
	for {
		timer := time.NewTimer(observestats.UntilNextMinute(w.now()))
		select {
		case <-timer.C:
			w.check(w.now(), stop)
		case <-stop:
			timer.Stop()
			return
		}
	}
}

// check evaluates every configured budget scope against the current local
// month's accumulated equivalent cost and fires alerts for crossed
// thresholds. Query or pricing failures are logged and retried next tick.
func (w *Watcher) check(now time.Time, stop <-chan struct{}) {
	// BudgetState captures reload-owned state once as copies; the stats store
	// and pricing carry their own leaf locks, so nothing nests under the
	// composition root's lock.
	budgets, parentOf, ok := w.ports.BudgetState()
	if !ok {
		return
	}

	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	month := monthStart.Format("2006-01")
	buckets, err := w.ports.QueryAnalytics(monthStart.Unix(), now.Unix())
	if err != nil {
		logx.Warnf("[budget] stats query failed: %v", err)
		return
	}
	// Same pricing snapshot as the analytics read view: config overrides
	// first, then the cached catalog (nil/disabled = only overrides price).
	overrides, catalog := w.ports.PricingSnapshot()

	total := 0.0
	perProvider := map[string]float64{}
	for _, bucket := range buckets {
		entry, ok := pricing.Resolve(overrides, catalog, bucket.Model)
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
		w.maybeAlert(alertPayload{
			Scope: "global", Month: month,
			ThresholdUSD: budgets.MonthlyUSD, ActualUSD: total,
		}, budgets.WebhookURL, now, stop)
	}
	for name, threshold := range budgets.Providers {
		if threshold <= 0 {
			continue
		}
		w.maybeAlert(alertPayload{
			Scope: name, Month: month,
			ThresholdUSD: threshold, ActualUSD: perProvider[name],
		}, budgets.WebhookURL, now, stop)
	}
}

// maybeAlert fires the live event and the optional webhook exactly once per
// (scope, month, threshold) per process. Crossing below the threshold again
// (e.g. after stats reset) does not re-arm the alert within the process.
func (w *Watcher) maybeAlert(payload alertPayload, webhookURL string, now time.Time, stop <-chan struct{}) {
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
		logx.Warnf("[budget] marshal alert payload failed: %v", err)
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
	w.ports.Publish(event)
	logx.Warnf("[budget] %s %s: actual $%.2f >= threshold $%.2f",
		payload.Scope, payload.Month, payload.ActualUSD, payload.ThresholdUSD)

	if webhookURL != "" {
		w.postWebhook(webhookURL, detail, stop)
	}
}

// postWebhook delivers the alert payload with up to webhookRetries retries on
// transport errors and 5xx. Failures are logged only — alerting never
// disturbs the main path. Both the retry sleep and the in-flight request
// abort when stop closes, so a hanging endpoint cannot pin the lifecycle wait
// (and the final flushes behind it) for the full retry budget.
func (w *Watcher) postWebhook(url string, body []byte, stop <-chan struct{}) {
	baseCtx, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()
	if stop != nil {
		go func() {
			select {
			case <-stop:
				baseCancel()
			case <-baseCtx.Done():
			}
		}()
	}
	for attempt := 0; attempt <= webhookRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(w.retryBackoff):
			case <-stop:
				return
			case <-baseCtx.Done():
				return
			}
		}
		ctx, cancel := context.WithTimeout(baseCtx, webhookTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			logx.Warnf("[budget] webhook request build failed: %v", err)
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
		if baseCtx.Err() != nil {
			return
		}
	}
	logx.Warnf("[budget] webhook POST %s failed after %d attempts", url, webhookRetries+1)
}
