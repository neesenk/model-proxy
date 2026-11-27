package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	runtimestate "model-proxy/internal/runtime"
)

// TestForward_Upstream403AttributionE2E pins the upstream-403 attribution end
// to end (contract: docs/architecture/routing-and-failure.md, 「普通转发的
// 403 归因」). A 403 is classified by its evidence, not by its status: a
// machine account-disablement code is an account fact (circuit tick +
// failover, like an exhausted 401 — and on the last candidate a proxy 502,
// never a pass-through of the account error); a recognised gateway refusal is
// no provider's fault (failover only; the last candidate keeps the raw 403);
// anything else, a generic HTML page included, is a plain 4xx commit.
func TestForward_Upstream403AttributionE2E(t *testing.T) {
	const (
		nginxDenial = "<html>\n<head><title>403 Forbidden</title></head>\n<body>\n<center><h1>403 Forbidden</h1></center>\n<hr><center>nginx</center>\n</body>\n</html>"
		genericHTML = "<html><title>403 Forbidden</title></html>"
		disabled    = `{"error":{"code":"account_disabled","message":"account closed"}}`
		suspended   = `{"error":{"code":"account_suspended","message":"account suspended"}}`
		fallback    = `{"choices":[{"message":{"role":"assistant","content":"fallback"}}]}`
	)
	htmlDenial := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, body)
		}
	}

	type rig struct {
		proxy *Proxy
		ups   []*fakeUpstream
		send  func() (int, string)
	}
	// newRig builds one route whose targets answer, in priority order, with
	// the given responders; they are named a, b, … by position.
	newRig := func(t *testing.T, responders ...http.HandlerFunc) rig {
		t.Helper()
		cfg := &configdomain.Config{
			Providers: map[string]configdomain.Provider{},
			Routes:    map[string][]configdomain.RouteTarget{},
		}
		keys := map[string]string{}
		ups := make([]*fakeUpstream, 0, len(responders))
		for i, respond := range responders {
			name := string(rune('a' + i))
			ups = append(ups, newFakeUpstream(t, respond))
			cfg.Providers[name] = configdomain.Provider{OpenAIBaseURL: ups[i].srv.URL, Provider: testProviderID}
			cfg.Routes["m1"] = append(cfg.Routes["m1"], configdomain.RouteTarget{Provider: name, Model: "m1", Priority: i + 1})
			keys[name] = name
		}
		proxy := newProxyWithStatic(t, cfg, keys)
		px := httptest.NewServer(http.HandlerFunc(proxy.Handler))
		t.Cleanup(px.Close)
		return rig{proxy: proxy, ups: ups, send: func() (int, string) {
			return post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)
		}}
	}
	accountFailures := func(p *Proxy, provider string) int {
		status, ok := p.runtimeState.Dashboard(time.Now()).Providers[provider]
		if !ok {
			return 0
		}
		return status.ConsecutiveFailures
	}
	metricsOf := func(p *Proxy, provider string) counters.ProviderMetricsSnapshot {
		return p.metrics.Snapshot()[counters.PMKey{Provider: provider, Model: "m1"}]
	}

	t.Run("account disablement fails over and ticks the account circuit", func(t *testing.T) {
		r := newRig(t, staticResponder(http.StatusForbidden, disabled), staticResponder(http.StatusOK, fallback))
		code, body := r.send()
		if code != http.StatusOK || !strings.Contains(body, "fallback") {
			t.Fatalf("status = %d body = %s, want the fallback target's 200", code, body)
		}
		if r.ups[0].hits() != 1 || r.ups[1].hits() != 1 {
			t.Errorf("hits = %d/%d, want 1/1", r.ups[0].hits(), r.ups[1].hits())
		}
		if got := accountFailures(r.proxy, "a"); got != 1 {
			t.Errorf("account failures for a = %d, want 1 (disablement is an account fact)", got)
		}
		if m := metricsOf(r.proxy, "a"); m.Failovers != 1 || m.Failures != 0 {
			t.Errorf("metrics for a = failovers %d / failures %d, want 1/0 (circuit only, like an exhausted 401)", m.Failovers, m.Failures)
		}
		if r.proxy.modelLocked("a", "m1", time.Now()) {
			t.Error("model locked: an account fact must not lock (provider, model)")
		}
	})

	t.Run("gateway refusal fails over without any failure accounting", func(t *testing.T) {
		r := newRig(t, htmlDenial(nginxDenial), staticResponder(http.StatusOK, fallback))
		code, body := r.send()
		if code != http.StatusOK || !strings.Contains(body, "fallback") {
			t.Fatalf("status = %d body = %s, want the fallback target's 200", code, body)
		}
		if got := accountFailures(r.proxy, "a"); got != 0 {
			t.Errorf("account failures for a = %d, want 0 (a gateway refusal is not the account's failure)", got)
		}
		if m := metricsOf(r.proxy, "a"); m.Failovers != 1 || m.Failures != 0 {
			t.Errorf("metrics for a = failovers %d / failures %d, want 1/0", m.Failovers, m.Failures)
		}
		if r.proxy.modelLocked("a", "m1", time.Now()) {
			t.Error("model locked after a gateway refusal")
		}
	})

	t.Run("last-candidate gateway refusal keeps the upstream 403", func(t *testing.T) {
		r := newRig(t, htmlDenial(nginxDenial))
		code, body := r.send()
		if code != http.StatusForbidden || body != nginxDenial {
			t.Errorf("status = %d body = %q, want the upstream 403 and its page passed through", code, body)
		}
		if got := accountFailures(r.proxy, "a"); got != 0 {
			t.Errorf("account failures for a = %d, want 0", got)
		}
	})

	t.Run("generic HTML 403 is committed as an ordinary 4xx", func(t *testing.T) {
		r := newRig(t, htmlDenial(genericHTML))
		code, body := r.send()
		if code != http.StatusForbidden || body != genericHTML {
			t.Errorf("status = %d body = %q, want the generic 403 committed as-is", code, body)
		}
		if got := accountFailures(r.proxy, "a"); got != 0 {
			t.Errorf("account failures for a = %d, want 0 (an HTML content type alone proves nothing)", got)
		}
		if r.proxy.modelLocked("a", "m1", time.Now()) {
			t.Error("model locked after a generic 403")
		}
	})

	t.Run("last-candidate account disablement ends in the proxy 502", func(t *testing.T) {
		r := newRig(t, staticResponder(http.StatusForbidden, suspended))
		code, body := r.send()
		if code != http.StatusBadGateway || !strings.Contains(body, "all targets failed") {
			t.Errorf("status = %d body = %q, want the 502 terminal (the account error is not passed through)", code, body)
		}
		if got := accountFailures(r.proxy, "a"); got != 1 {
			t.Errorf("account failures for a = %d, want 1", got)
		}
	})
}

// TestForward_GatewayRefusalStormOpensCircuit pins the 2026-11-26 storm. A
// network switch put compass/aqp behind the gateway's security policy for
// four hours; every call to those accounts came back as the stock nginx 403
// page, and because a refusal was accounted nowhere nothing ever opened — 188
// requests paid 479 failed legs walking the refused accounts first. The
// refusal is about the network path, not the key: it is never a credential
// problem (no denial, no banner) and never a failure metric — but six in a
// row open the account's circuit, and from then on a request skips the
// account without a round trip and goes straight to the sibling.
func TestForward_GatewayRefusalStormOpensCircuit(t *testing.T) {
	logs := captureServeLog(t)
	const (
		nginxDenial = "<html>\n<head><title>403 Forbidden</title></head>\n<body>\n<center><h1>403 Forbidden</h1></center>\n<hr><center>nginx</center>\n</body>\n</html>"
		fallback    = `{"choices":[{"message":{"role":"assistant","content":"fallback"}}]}`
	)
	refusing := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, nginxDenial)
	})
	sibling := newFakeUpstream(t, staticResponder(http.StatusOK, fallback))
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: refusing.srv.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: sibling.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"m1": {
			{Provider: "a", Model: "m1", Priority: 1},
			{Provider: "b", Model: "m1", Priority: 2},
		}},
		// persist 0: the refusal count alone opens (the span gate is the
		// runtime package's to pin); a long cooldown keeps it open here.
		Scheduling: configdomain.Scheduling{CircuitPersist: "0", CircuitCooldown: "1h"},
	}
	proxy := newProxyWithStatic(t, cfg, map[string]string{"a": "a", "b": "b"})
	px := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(px.Close)
	send := func(what string) {
		t.Helper()
		code, body := post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)
		if code != http.StatusOK || !strings.Contains(body, "fallback") {
			t.Fatalf("%s: status = %d body = %s, want the sibling's 200", what, code, body)
		}
	}
	healthy := func() bool { return proxy.runtimeState.TargetHealthy("a", "m1", time.Now()) }

	// Below the threshold the account keeps taking its turn first.
	for i := 1; i < runtimestate.GatewayRefusalThreshold; i++ {
		send(fmt.Sprintf("refusal %d", i))
	}
	if got := refusing.hits(); got != runtimestate.GatewayRefusalThreshold-1 {
		t.Fatalf("refused upstream hits = %d, want %d", got, runtimestate.GatewayRefusalThreshold-1)
	}
	if !healthy() {
		t.Fatalf("%d consecutive refusals made the account unavailable; the threshold is %d. state=%+v modelLocked=%v concurrency=%+v\n%s",
			runtimestate.GatewayRefusalThreshold-1, runtimestate.GatewayRefusalThreshold,
			proxy.runtimeState.Dashboard(time.Now()).Providers["a"], proxy.modelLocked("a", "m1", time.Now()), proxy.runtimeState.Concurrency("a"), logs.text())
	}

	// The sixth refusal opens it…
	send("the refusal that opens the circuit")
	if healthy() {
		t.Fatalf("%d consecutive refusals did not open the circuit", runtimestate.GatewayRefusalThreshold)
	}
	if status, ok := proxy.runtimeState.Dashboard(time.Now()).Providers["a"]; !ok || status.CircuitState != "open" {
		t.Fatalf("circuit state of a = %+v, want open", status)
	}

	// …and the next request no longer pays for it: no round trip to the
	// refused account, the sibling answers straight away. (An open circuit is
	// filtered at scheduling — the account simply is not a candidate any
	// more; the executor's `skipped (circuit open …)` line belongs to a
	// circuit that opens between scheduling and dispatch.)
	send("the request after the circuit opened")
	if got := refusing.hits(); got != runtimestate.GatewayRefusalThreshold {
		t.Fatalf("refused upstream hits = %d, want it untouched at %d once its circuit is open", got, runtimestate.GatewayRefusalThreshold)
	}
	if got := sibling.hits(); got != runtimestate.GatewayRefusalThreshold+1 {
		t.Fatalf("sibling hits = %d, want every request served", got)
	}
	for _, line := range strings.Split(logs.text(), "\n") {
		if strings.Contains(line, "credential") || strings.Contains(line, "denied the account") {
			t.Errorf("a gateway refusal was logged as a credential problem: %s", line)
		}
	}

	// A refusal is never the account's failure and never its credential.
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "a", Model: "m1"}]; m.Failures != 0 {
		t.Errorf("failure metric for a = %d, want 0 (a gateway refusal is not a failure)", m.Failures)
	}
	if status := proxy.runtimeState.Dashboard(time.Now()); status.Providers["a"].ConsecutiveFailures != 0 || len(status.CredentialDenials) != 0 {
		t.Errorf("a = %+v denials = %+v, want no hard failure and no credential denial", status.Providers["a"], status.CredentialDenials)
	}
	if proxy.modelLocked("a", "m1", time.Now()) {
		t.Error("model locked: a gateway refusal must not lock (provider, model)")
	}
}
