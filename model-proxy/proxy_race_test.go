package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestForward_CfgReadNoRaceWithReload runs the request hot path concurrently
// with reload (which writes p.cfg under mu). schedule/tryTarget/billingClass/
// effectiveRemaining must read the cfg snapshot threaded down from forward, not
// p.cfg directly — otherwise this is a data race (and a snapshot-inconsistency
// bug: forward's targets come from the old cfg while schedule reads the new one).
// The race detector only sees it when a reload overlaps a request, which no other
// test exercises. Run with -race.
func TestForward_CfgReadNoRaceWithReload(t *testing.T) {
	useStaticProviderPools(t, "p")
	// Alternate 200/500 so the test exercises both recordSuccess and
	// recordFailure (both reachable from tryTarget during a request).
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		if hits.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgYAML := "listen: 127.0.0.1:0\n" +
		"providers:\n  p:\n    openai_base_url: " + upstream.URL + "\n    provider_id: static\n" +
		"routes:\n  m:\n    - {provider: p, model: m}\n" +
		"scheduling:\n  sticky_dwell: 0s\n  upstream_timeout: 1s\n  circuit_threshold: 10\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxyAt(t, cfg, filepath.Join(dir, "quota_state.json"))
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	reloadErrs := make(chan error, 50)
	var served, failed, unexpected, transportErrors atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m","messages":[]}`))
				if err != nil {
					transportErrors.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					served.Add(1)
				case http.StatusBadGateway:
					failed.Add(1)
				default:
					unexpected.Add(1)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := p.reload(cfgPath); err != nil {
				reloadErrs <- err
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(reloadErrs)
	for err := range reloadErrs {
		t.Errorf("reload: %v", err)
	}
	if got := transportErrors.Load(); got != 0 {
		t.Errorf("request transport errors = %d, want 0", got)
	}
	if got := unexpected.Load(); got != 0 {
		t.Errorf("unexpected HTTP statuses = %d, want 0", got)
	}
	if served.Load() == 0 || failed.Load() == 0 {
		t.Errorf("completed requests 200=%d 502=%d, want both paths exercised during reload", served.Load(), failed.Load())
	}
	if got := hits.Load(); got == 0 {
		t.Error("upstream received no requests; reload race test exercised no forwarding")
	}
}
