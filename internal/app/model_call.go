// model_call.go — the scheduling seam for one-shot (non-streaming) model
// calls made by internal owners (guard adjudication today). Layering:
//
//	adjudicationCaller (judge domain: prompt, verdict parsing)
//	  → modelExchange (this seam: ordering, cooldown skip, per-attempt
//	    budgets, health recording) → probe.Do (transport recipe)
//
// Scheduling goes through the SAME Manager path the forward pipeline uses
// (p.schedule → Manager.DecideOrder: tier→priority→score, circuit-open /
// rate-limited / model-locked / quota-exhausted targets skipped, pins
// honored), and outcomes are recorded back into the shared health state —
// the judge leg and forward traffic see one provider-health view. Transport
// deliberately stays probe.Do and never enters the forward transport layer:
// the judged snippet would re-trigger the guard on itself, and internal
// adjudication traffic must not pollute the request log, cache or forward
// stats (decision 36 ②).
package app

import (
	"context"
	"encoding/json"
	"fmt"
	configdomain "model-proxy/internal/config"
	"net/http"
	"time"

	"model-proxy/internal/forward"
	"model-proxy/internal/probe"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
)

// modelExchangeMaxAttempts bounds one exchange call's failover depth. The
// availability-filtered order usually makes the first capable target decide.
const modelExchangeMaxAttempts = 3

// modelExchange is one scheduled /v1/messages exchange against an exposed
// route name. body arrives WITHOUT a model field — the exchange rewrites it
// per target (per-target upstream ids differ). validate is the caller-domain
// reply check (e.g. the judge's verdict-JSON contract): a non-nil error
// fails over to the next target exactly like a transport failure would.
type modelExchange interface {
	exchange(ctx context.Context, snap forward.Snapshot, model string, body []byte,
		validate func(exchangeResult) error) (exchangeResult, error)
}

// exchangeResult carries the winning attempt's identity and raw reply.
type exchangeResult struct {
	Provider string
	Model    string
	Status   int
	Body     []byte
}

// scheduledExchange routes the call through the shared scheduling and health
// machinery (see the package comment).
type scheduledExchange struct {
	p *Proxy
}

// exchangeSessionKey is the sticky-slot key for headless model calls: it
// keeps the judge parked on one provider for the dwell window (latency- and
// quota-friendly) without touching any client session's sticky slot.
func exchangeSessionKey(purpose, model string) string {
	return purpose + ":" + model
}

func (x scheduledExchange) exchange(ctx context.Context, snap forward.Snapshot, model string, body []byte,
	validate func(exchangeResult) error) (exchangeResult, error) {
	if snap.Cfg == nil {
		return exchangeResult{}, fmt.Errorf("model exchange: nil snapshot")
	}
	targets := snap.ExpandedRoutes[model]
	if len(targets) == 0 {
		return exchangeResult{}, fmt.Errorf("adjudicate model %q has no route", model)
	}
	ordered := x.p.schedule(snap.Cfg, snap.ParentOf, model,
		exchangeSessionKey("guard-adjudicate", model), targets, snap.RouteKeys, snap.Generation)
	// The exchange speaks the anthropic leg only: keep targets whose provider
	// has an anthropic_base_url and a live impl, in scheduled order.
	capable := make([]configdomain.RouteTarget, 0, len(ordered))
	for _, t := range ordered {
		provCfg, okCfg := snap.Cfg.Providers[t.Provider]
		if !okCfg || provCfg.AnthropicBaseURL == "" {
			continue
		}
		if _, okImpl := snap.Providers[t.Provider]; !okImpl {
			continue
		}
		capable = append(capable, t)
	}
	if len(capable) == 0 {
		if len(ordered) == 0 {
			// Every target was skipped by the availability filter (circuit
			// open / rate limited / model locked / quota exhausted): fail
			// fast — the caller's fail-open path takes over.
			return exchangeResult{}, fmt.Errorf("adjudicate model %q: all %d route targets unavailable (cooldown)", model, len(targets))
		}
		return exchangeResult{}, fmt.Errorf("adjudicate model %q: no route target with an anthropic_base_url", model)
	}
	attempts := len(capable)
	if attempts > modelExchangeMaxAttempts {
		attempts = modelExchangeMaxAttempts
	}
	var lastErr error
	for i, t := range capable {
		if i >= attempts {
			break
		}
		provCfg := snap.Cfg.Providers[t.Provider]
		impl := snap.Providers[t.Provider]
		targetBody, err := withModelField(body, t.Model)
		if err != nil {
			return exchangeResult{}, err
		}
		// Per-attempt budget: the FIRST attempt (the schedule's best target)
		// gets the dominant share — a healthy but slow judge must be able to
		// finish within its attempt — while every remaining attempt keeps a
		// reserve, so a hanging or degraded target cannot consume the whole
		// call budget (the failure mode that used to starve the channel).
		actx, cancel := sliceAttemptContext(ctx, attempts-i)
		rep, err := probe.Do(actx, x.p.client, provCfg, impl, probe.Request{
			BaseURL: provCfg.AnthropicBaseURL,
			Path:    "/v1/messages",
			Body:    targetBody,
		})
		cancel()
		// The outer budget expiring is the caller's cancel, not this leg's
		// fault: record nothing and stop the chain (the client-cancel rule —
		// a dead budget must not burn circuit ticks on the remaining targets
		// either).
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = fmt.Errorf("adjudication call (%s): %w", t.Provider, ctx.Err())
			}
			break
		}
		if err != nil {
			// A per-attempt deadline or transport error with the outer budget
			// still alive IS the leg failing — it feeds the shared breaker.
			x.p.recordFailure(t.Provider, snap.Cfg.Scheduling, snap.Generation)
			lastErr = fmt.Errorf("adjudication call (%s): %w", t.Provider, err)
			continue
		}
		if rep.Status == http.StatusTooManyRequests {
			// Same single classification entry as the executor and Fusion
			// legs (routing-and-failure.md). probe replies carry no response
			// headers, so Retry-After cannot be honored — body reset hints
			// and the scheduling defaults still apply.
			decision := targetexec.ParseRateLimit(nil, rep.Body, time.Now(), snap.Cfg.Scheduling)
			x.p.recordRateLimit(t.Provider, decision.Until,
				runtimestate.ParseRateLimitKind(string(decision.Kind)), snap.Generation)
			lastErr = fmt.Errorf("adjudication call (%s): status 429 (%s) — cooling until %s",
				t.Provider, decision.Kind, decision.Until.Format(time.Kitchen))
			continue
		}
		if rep.Status >= 500 {
			x.p.recordFailure(t.Provider, snap.Cfg.Scheduling, snap.Generation)
			lastErr = fmt.Errorf("adjudication call (%s): status %d: %s", t.Provider, rep.Status, truncateAdjudication(string(rep.Body), 200))
			continue
		}
		if rep.Status == 404 || targetexec.IsModelDenied(rep.Status, rep.Body) {
			// Model-level denial locks only this (provider, model) leg — the
			// provider's other routes stay schedulable.
			x.p.recordModelFailure(t.Provider, t.Model, snap.Cfg.Scheduling, snap.Generation)
			lastErr = fmt.Errorf("adjudication call (%s): status %d: %s", t.Provider, rep.Status, truncateAdjudication(string(rep.Body), 200))
			continue
		}
		if rep.Status >= 300 {
			// Other 4xx: the request shape or auth was rejected — not a
			// provider-health signal (the executor likewise commits unhandled
			// 4xx without a health write).
			lastErr = fmt.Errorf("adjudication call (%s): status %d: %s", t.Provider, rep.Status, truncateAdjudication(string(rep.Body), 200))
			continue
		}
		if validate != nil {
			if verr := validate(exchangeResult{Provider: t.Provider, Model: t.Model, Status: rep.Status, Body: rep.Body}); verr != nil {
				// The model answered but the caller's reply contract failed:
				// a model-level fault, mirrored from the executor's
				// empty-200 handling.
				x.p.recordModelFailure(t.Provider, t.Model, snap.Cfg.Scheduling, snap.Generation)
				lastErr = fmt.Errorf("adjudication reply (%s): %w", t.Provider, verr)
				continue
			}
		}
		x.p.recordSuccess(t.Provider, t.Model, snap.Generation)
		return exchangeResult{Provider: t.Provider, Model: t.Model, Status: rep.Status, Body: rep.Body}, nil
	}
	return exchangeResult{}, lastErr
}

// modelExchangeReserve is the per-attempt floor kept aside for each attempt
// after the current one: enough for a healthy provider to answer a terse
// judge prompt (thinking disabled, ~2s measured, seconds of headroom), so a
// fallback target still gets a viable turn after a slow or hanging first
// attempt. It never exceeds the equal share of the remaining budget.
const modelExchangeReserve = 4 * time.Second

// sliceAttemptContext derives the per-attempt budget: the current attempt
// gets the remaining time minus a reserve for each later attempt (the first,
// best-scheduled target gets the dominant share — a legitimately slow judge
// must fit; later attempts are fallbacks on smaller remainders). Without a
// deadline on ctx the parent passes through; the last attempt keeps whatever
// is left — no artificial cutoff ahead of the outer budget.
func sliceAttemptContext(ctx context.Context, attemptsLeft int) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || attemptsLeft <= 1 {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	share := remaining / time.Duration(attemptsLeft)
	if share <= 0 {
		return context.WithCancel(ctx)
	}
	reserve := modelExchangeReserve
	if reserve > share {
		reserve = share
	}
	budget := remaining - reserve*time.Duration(attemptsLeft-1)
	if budget <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}

// withModelField rewrites the marshaled prompt body's model field onto one
// target's upstream model id (the route's per-target names differ).
func withModelField(prompt []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(prompt, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	return json.Marshal(m)
}
