// Command soak is a closed-loop load harness for a running model-proxy: it
// drives reproducible request scenarios (short interactive, streaming,
// long-context, tool traffic, prefix reuse, error storms, client
// cancellation, and a mix of all) against one base URL and reports per-
// scenario counts, error rates, and latency percentiles.
//
// It sends REAL requests — whatever route the -model name resolves to pays
// for real upstream traffic. Point -base-url at a proxy whose route targets a
// cheap/mock backend, or at the dev server, before running long durations.
//
// Usage:
//
//	go run ./scripts/soak -base-url http://127.0.0.1:15722 -model glm -duration 30s -concurrency 4
//	go run ./scripts/soak -base-url ... -scenario cancel        # one scenario
//	go run ./scripts/soak -base-url ... -max-error-rate 0.02    # exit 1 above it
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type scenario struct {
	name string
	desc string
	// build returns the request body for iteration i (the index keeps
	// prefix-reuse and rotating traffic reproducible).
	build func(model string, i int) string
	// stream selects SSE requests (body read to EOF, TTFT measured).
	stream bool
	// cancelAfter aborts the request mid-flight when > 0 (client-
	// cancellation path); the abort counts as ok.
	cancelAfter time.Duration
	// expectStatus, when non-zero, inverts the error verdict: that status is
	// the expected outcome (error-storm scenarios must not be reported as
	// failures).
	expectStatus int
}

func scenarios() []scenario {
	padding := strings.Repeat("x", 48*1024)
	return []scenario{
		{
			name: "short", desc: "small non-streaming anthropic requests",
			build: func(model string, i int) string {
				return fmt.Sprintf(`{"model":%q,"max_tokens":64,"messages":[{"role":"user","content":"ping %d"}]}`, model, i)
			},
		},
		{
			name: "stream", desc: "SSE requests drained to EOF (TTFT measured)",
			build: func(model string, i int) string {
				return fmt.Sprintf(`{"model":%q,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"stream %d"}]}`, model, i)
			},
			stream: true,
		},
		{
			name: "longctx", desc: "≈48KiB prompt bodies (context-estimation path)",
			build: func(model string, i int) string {
				return fmt.Sprintf(`{"model":%q,"max_tokens":32,"messages":[{"role":"user","content":"context %d %s"}]}`, model, i, padding)
			},
		},
		{
			name: "tools", desc: "tool definitions + tool_use/tool_result history",
			build: func(model string, i int) string {
				return fmt.Sprintf(`{"model":%q,"max_tokens":64,"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],`+
					`"messages":[{"role":"user","content":"lookup %d"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_%d","name":"lookup","input":{"q":"x"}}]},`+
					`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":[{"type":"text","text":"ok"}]}]}]}`, model, i, i, i)
			},
		},
		{
			name: "prefix", desc: "byte-identical bodies every iteration (cache/connection reuse)",
			build: func(model string, i int) string {
				return fmt.Sprintf(`{"model":%q,"max_tokens":32,"messages":[{"role":"user","content":"constant prompt"}]}`, model)
			},
		},
		{
			name: "errors", desc: "unknown-model requests (502 storm on the error path)",
			build: func(model string, i int) string {
				return `{"model":"soak-missing-model","messages":[{"role":"user","content":"x"}]}`
			},
			expectStatus: http.StatusBadGateway,
		},
		{
			name: "cancel", desc: "streams aborted after 200ms (client-disconnect path)",
			build: func(model string, i int) string {
				return fmt.Sprintf(`{"model":%q,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"cancel %d"}]}`, model, i)
			},
			stream:      true,
			cancelAfter: 200 * time.Millisecond,
		},
	}
}

type result struct {
	ok     bool
	status int
	ms     float64
	ttftMS float64 // streaming only; total latency otherwise
	// aborted marks requests cut by the scenario deadline itself — they say
	// nothing about the proxy and are excluded from the statistics.
	aborted bool
}

func runScenario(ctx context.Context, client *http.Client, baseURL, model, name string, s scenario, concurrency int) []result {
	scenarioCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup
	next := make(chan int, concurrency*2)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-scenarioCtx.Done():
				close(next)
				return
			case next <- i:
			}
		}
	}()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if scenarioCtx.Err() != nil {
					return
				}
				if r := oneRequest(scenarioCtx, client, baseURL, s, model, i); !r.aborted {
					mu.Lock()
					results = append(results, r)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return results
}

func oneRequest(ctx context.Context, client *http.Client, baseURL string, s scenario, model string, i int) result {
	reqCtx := ctx
	var cancel context.CancelFunc
	if s.cancelAfter > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, s.cancelAfter)
		defer cancel()
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+"/v1/messages", strings.NewReader(s.build(model, i)))
	if err != nil {
		return result{ok: false, ms: msSince(start)}
	}
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// A client-side cancel counts as the expected outcome of the cancel
		// scenario, not as a proxy failure — but only when OUR per-request
		// timeout is what fired. The scenario/run deadline expiring
		// propagates the same reqCtx.Err() and must stay "aborted"
		// (excluded from stats), not silently count as ok.
		if s.cancelAfter > 0 && ctx.Err() == nil && reqCtx.Err() != nil {
			return result{ok: true, status: 0, ms: msSince(start)}
		}
		if reqCtx.Err() != nil {
			return result{aborted: true, ms: msSince(start)}
		}
		return result{ok: false, ms: msSince(start)}
	}
	defer resp.Body.Close()
	// Streaming gauge: time to the first body byte, then drain.
	ttft := 0.0
	if s.stream {
		one := make([]byte, 1)
		if _, err := resp.Body.Read(one); err == nil {
			ttft = msSince(start)
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	total := msSince(start)
	if s.cancelAfter > 0 {
		// The cancel scenario measures the disconnect path: a request that
		// completed before its cancel timer is fine regardless of status
		// (the failure gate excludes this scenario by design).
		return result{ok: true, status: resp.StatusCode, ms: total, ttftMS: ttft}
	}
	ok := resp.StatusCode == http.StatusOK
	if s.expectStatus != 0 {
		ok = resp.StatusCode == s.expectStatus
	}
	return result{ok: ok, status: resp.StatusCode, ms: total, ttftMS: ttft}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

func report(name string, results []result) (errorRate float64) {
	var oks, errs int
	lat, ttft := make([]float64, 0, len(results)), make([]float64, 0, len(results))
	statusCount := map[int]int{}
	for _, r := range results {
		if r.ok {
			oks++
		} else {
			errs++
			statusCount[r.status]++
		}
		lat = append(lat, r.ms)
		if r.ttftMS > 0 {
			ttft = append(ttft, r.ttftMS)
		}
	}
	sort.Float64s(lat)
	sort.Float64s(ttft)
	if oks+errs > 0 {
		errorRate = float64(errs) / float64(oks+errs)
	}
	fmt.Printf("%-8s ok=%-6d err=%-4d rate=%5.1f%%  p50=%7.1fms p95=%7.1fms p99=%7.1fms",
		name, oks, errs, errorRate*100,
		percentile(lat, 0.50), percentile(lat, 0.95), percentile(lat, 0.99))
	if len(ttft) > 0 {
		fmt.Printf("  ttft p50=%7.1fms p95=%7.1fms", percentile(ttft, 0.50), percentile(ttft, 0.95))
	}
	if len(statusCount) > 0 {
		parts := make([]string, 0, len(statusCount))
		codes := make([]int, 0, len(statusCount))
		for code := range statusCount {
			codes = append(codes, code)
		}
		sort.Ints(codes)
		for _, code := range codes {
			parts = append(parts, fmt.Sprintf("%d×%d", code, statusCount[code]))
		}
		fmt.Printf("  statuses=%s", strings.Join(parts, ","))
	}
	fmt.Println()
	return errorRate
}

func main() {
	baseURL := flag.String("base-url", "http://127.0.0.1:15721", "proxy base URL")
	model := flag.String("model", "glm", "model name to route (as exposed by the proxy)")
	duration := flag.Duration("duration", 30*time.Second, "total run duration (split across scenarios)")
	concurrency := flag.Int("concurrency", 4, "workers per scenario")
	only := flag.String("scenario", "mix", "scenario name, or \"mix\" for all, or \"list\"")
	maxErr := flag.Float64("max-error-rate", 0.02, "exit 1 when a non-error scenario exceeds this rate")
	flag.Parse()

	all := scenarios()
	if *only == "list" {
		for _, s := range all {
			fmt.Printf("%-8s %s\n", s.name, s.desc)
		}
		return
	}
	selected := all
	if *only != "mix" {
		selected = nil
		for _, s := range all {
			if s.name == *only {
				selected = append(selected, s)
			}
		}
		if len(selected) == 0 {
			fmt.Fprintf(os.Stderr, "unknown scenario %q (use -scenario list)\n", *only)
			os.Exit(2)
		}
	}
	perScenario := *duration / time.Duration(len(selected))
	fmt.Printf("soak %s model=%s concurrency=%d per-scenario=%s\n", *baseURL, *model, *concurrency, perScenario)
	client := &http.Client{Timeout: 10 * time.Minute}
	failed := false
	for _, s := range selected {
		ctx, cancel := context.WithTimeout(context.Background(), perScenario)
		results := runScenario(ctx, client, *baseURL, *model, s.name, s, *concurrency)
		cancel()
		rate := report(s.name, results)
		if s.expectStatus == 0 && s.cancelAfter == 0 && rate > *maxErr {
			failed = true
		}
	}
	if failed {
		fmt.Println("FAIL: error rate above threshold")
		os.Exit(1)
	}
	fmt.Println("soak complete")
}
