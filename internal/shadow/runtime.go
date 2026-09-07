// Package shadow owns the detached, reload-swappable transport used by
// best-effort Shadow dispatch. Routing, lifecycle admission, and request-log
// policy remain in the composition root.
package shadow

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"model-proxy/internal/targetexec"
	"model-proxy/internal/transport/bodycapture"
)

const defaultMaxConcurrent = 4

var (
	errNoRuntime  = errors.New("shadow runtime is unavailable")
	errNoProvider = errors.New("shadow provider is unavailable")
)

// Options configures one immutable, reload-scoped dispatch runtime.
//
// A nil SampleRate samples every eligible request. MaxConcurrent defaults to
// four when it is zero or negative. Timeout is passed directly to the
// runtime-owned HTTP client.
type Options struct {
	SampleRate    *float64
	MaxConcurrent int
	Timeout       time.Duration
}

// Runtime contains only the detached resources that must remain consistent for
// one admitted Shadow task. A reload replaces the whole Runtime; in-flight
// callers keep their captured instance.
type Runtime struct {
	sem        chan struct{}
	client     *http.Client
	sampleRate float64
	random     func() float64
	// dropped counts TryAcquire calls the concurrency gate actually rejected
	// (semaphore full). It observes the gate drop rate and doubles as a
	// deterministic seam for concurrency tests: once Dropped increments, the
	// dispatch under test provably ran while the gate was saturated.
	dropped atomic.Int64
}

// NewRuntime builds a reload-swappable Shadow runtime.
func NewRuntime(options Options) *Runtime {
	return newRuntime(options, nil)
}

// newRuntime lets same-package tests inject deterministic sampling without
// exposing a test hook in the production API.
func newRuntime(options Options, random func() float64) *Runtime {
	maxConcurrent := options.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	sampleRate := 1.0
	if options.SampleRate != nil {
		sampleRate = *options.SampleRate
	}
	if random == nil {
		random = rand.Float64
	}
	return &Runtime{
		sem:        make(chan struct{}, maxConcurrent),
		client:     &http.Client{Timeout: options.Timeout},
		sampleRate: sampleRate,
		random:     random,
	}
}

// ShouldSample decides whether this runtime admits a request to Shadow. The
// semaphore is deliberately separate: root samples, then TryAcquire, then
// performs lifecycle admission and releases the slot if admission is rejected.
func (runtime *Runtime) ShouldSample() bool {
	if runtime == nil || runtime.sem == nil {
		return false
	}
	if runtime.sampleRate >= 1 {
		return true
	}
	if runtime.sampleRate <= 0 {
		return false
	}
	return runtime.random() < runtime.sampleRate
}

// Permit is one successfully acquired detached Shadow slot. Release is
// idempotent, so a duplicated cleanup path cannot free another task's slot.
type Permit struct {
	runtime *Runtime
	once    sync.Once
}

// TryAcquire reserves one detached Shadow slot without blocking. A nil return
// means that no slot is available; only that genuine gate rejection is counted
// in dropped (the nil-runtime / nil-semaphore paths are not).
func (runtime *Runtime) TryAcquire() *Permit {
	if runtime == nil || runtime.sem == nil {
		return nil
	}
	select {
	case runtime.sem <- struct{}{}:
		return &Permit{runtime: runtime}
	default:
		runtime.dropped.Add(1)
		return nil
	}
}

// Dropped reports how many dispatches the concurrency gate has rejected. A nil
// runtime reports zero.
func (runtime *Runtime) Dropped() int64 {
	if runtime == nil {
		return 0
	}
	return runtime.dropped.Load()
}

// Release returns this permit's slot at most once.
func (permit *Permit) Release() {
	if permit == nil || permit.runtime == nil || permit.runtime.sem == nil {
		return
	}
	permit.once.Do(func() {
		<-permit.runtime.sem
	})
}

// Job is the root-prepared input to one detached Shadow execution. Body is the
// primary attempt's committed upstream body; Execute never mutates it.
type Job struct {
	Plan        targetexec.Plan
	Body        []byte
	CalledModel string
	// Client optionally overrides the runtime default client. internal/app
	// sets it so the shadow request follows the shadow provider's proxy chain
	// (providers.<name>.proxy_url → global proxy → env → system) with shadow's
	// own timeout budget. Nil keeps the runtime default.
	Client       *http.Client
	MaxBodyBytes int
}

// Capture is the bounded response prefix and its complete-stream metadata.
// Body is independent of the bodycapture callback buffer and may be retained.
type Capture struct {
	Body      []byte
	Total     int64
	Truncated bool
}

// Result contains the detached upstream exchange needed by root request-log
// mapping. Response.Body has been drained and closed before Execute returns.
// Err describes local preparation, transport, or response-drain failure.
type Result struct {
	Request     *http.Request
	RequestBody []byte
	Response    *http.Response
	Started     time.Time
	Capture     Capture
	Err         error
}

// Execute sends one best-effort Shadow request. It does not perform retries,
// lifecycle work, metrics, provider resolution, or logging. Conversion is
// fail-closed: a conversion error returns before any HTTP request is made.
func (runtime *Runtime) Execute(ctx context.Context, job Job) Result {
	result := Result{}
	if runtime == nil || runtime.client == nil {
		result.Err = errNoRuntime
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}

	provider := job.Plan.Provider()
	if provider == nil {
		result.Err = errNoProvider
		return result
	}

	// Providers are allowed to rewrite their body argument. Keep that mutation
	// fully detached from the committed primary request held by the root.
	body := append([]byte(nil), job.Body...)
	body = job.Plan.RewriteModel(body, job.CalledModel)
	var err error
	body, err = job.Plan.ConvertBody(body)
	if err != nil {
		result.Err = err
		return result
	}

	targetURL := strings.TrimRight(job.Plan.BaseURL(), "/") + job.Plan.UpstreamPath()
	targetURL, body = provider.RewriteRequest(targetURL, body, job.Plan.UpstreamPath())
	result.RequestBody = body
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		result.Err = err
		return result
	}
	req.Header.Set("Content-Type", "application/json")
	if err := provider.AuthHeaders(req); err != nil {
		result.Request = req
		result.Err = err
		return result
	}
	job.Plan.ApplyConfiguredHeaders(req.Header)
	provider.ExtraHeaders(req, job.Plan.UpstreamPath())

	result.Request = req
	result.Started = time.Now()
	client := runtime.client
	if job.Client != nil {
		client = job.Client
	}
	response, err := client.Do(req)
	result.Response = response
	if err != nil {
		result.Err = err
		return result
	}

	captured := bodycapture.New(response.Body, job.MaxBodyBytes, func(body []byte, total int64, truncated bool) {
		result.Capture = Capture{
			Body:      append([]byte(nil), body...),
			Total:     total,
			Truncated: truncated,
		}
	})
	_, copyErr := io.Copy(io.Discard, captured)
	closeErr := captured.Close()
	if copyErr != nil {
		result.Err = copyErr
	} else if closeErr != nil {
		result.Err = closeErr
	}
	return result
}
