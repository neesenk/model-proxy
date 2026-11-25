package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
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
