package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/admin"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/provider"
)

// TestForward_LapsedPlanSubscriptionFailsOverE2E pins the 2026-11-25 step-plan
// incident end to end, through the real handler and request log (contract:
// docs/architecture/routing-and-failure.md, 「普通转发的 403 归因」). An
// account whose plan subscription lapsed answers every request with HTTP 400
// {"error":{"message":"you have no active step plan subscription",
// "type":"request_params_invalid"}}. That is an account fact, not a verdict on
// the request: the leg fails over to the next target and the client gets that
// target's answer. The deployed build committed 11 of these 400s as terminal
// responses while both sibling accounts served the same route. The account
// takes one circuit tick and no failure metric (like an exhausted 401); on the
// last candidate the proxy answers with its own 502 instead of passing the
// account error through. The upstream body is the leg's request-log evidence
// either way.
func TestForward_LapsedPlanSubscriptionFailsOverE2E(t *testing.T) {
	const (
		lapsed   = `{"error":{"message":"you have no active step plan subscription","type":"request_params_invalid"}}`
		fallback = `{"choices":[{"message":{"role":"assistant","content":"fallback"}}]}`
	)
	// The wire shape observed in the incident: a JSON error body served as
	// text/plain.
	lapsedResponder := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, lapsed)
	}

	type rig struct {
		proxy *Proxy
		ups   []*fakeUpstream
		dir   string
		drain func()
		send  func() (int, string)
	}
	// newRig builds one route whose targets answer, in priority order, with
	// the given responders; they are named a, b, … by position. drain flushes
	// the request log so its rows can be queried.
	newRig := func(t *testing.T, loadBalance bool, responders ...http.HandlerFunc) rig {
		t.Helper()
		cfg := &configdomain.Config{
			Providers: map[string]configdomain.Provider{},
			Routes:    map[string][]configdomain.RouteTarget{},
		}
		if loadBalance {
			cfg.RouteStrategies = map[string]string{"m1": configdomain.RouteStrategyLoadBalance}
		}
		ups := make([]*fakeUpstream, 0, len(responders))
		for i, respond := range responders {
			name := string(rune('a' + i))
			ups = append(ups, newFakeUpstream(t, respond))
			cfg.Providers[name] = configdomain.Provider{OpenAIBaseURL: ups[i].srv.URL, Provider: testProviderID}
			cfg.Routes["m1"] = append(cfg.Routes["m1"], configdomain.RouteTarget{Provider: name, Model: "m1", Priority: i + 1})
		}
		proxy, dir, drain := newReqLogProxy(t, cfg)
		for i := range responders {
			name := string(rune('a' + i))
			proxy.providers[name] = &testProv{key: name}
		}
		server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
		t.Cleanup(server.Close)
		return rig{proxy: proxy, ups: ups, dir: dir, drain: drain, send: func() (int, string) {
			return post(t, server.URL+"/v1/chat/completions", `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`)
		}}
	}
	accountFailures := func(p *Proxy, provider string) int {
		status, ok := p.runtimeState.Dashboard(time.Now()).Providers[provider]
		if !ok {
			return 0
		}
		return status.ConsecutiveFailures
	}
	// assertAccountFact checks the attribution's footprint on one target: a
	// circuit tick per lapsed answer, no failure metric, no model lock.
	assertAccountFact := func(t *testing.T, r rig, i int, ticks int) {
		t.Helper()
		name := string(rune('a' + i))
		if got := accountFailures(r.proxy, name); got != ticks {
			t.Errorf("account failures for %s = %d, want %d (a lapsed subscription is an account fact)", name, got, ticks)
		}
		if m := r.proxy.metrics.Snapshot()[counters.PMKey{Provider: name, Model: "m1"}]; m.Failovers != uint64(ticks) || m.Failures != 0 {
			t.Errorf("metrics for %s = failovers %d / failures %d, want %d/0 (circuit only, like an exhausted 401)", name, m.Failovers, m.Failures, ticks)
		}
		if r.proxy.modelLocked(name, "m1", time.Now()) {
			t.Errorf("model locked on %s: an account fact must not lock (provider, model)", name)
		}
	}
	// assertLogEvidence checks the request log after drain: exactly one
	// answered row carrying the given terminal status/provider, with the
	// lapsed account's hop folded in, and exactly one leg record holding that
	// hop's observed 400 and the upstream error body.
	assertLogEvidence := func(t *testing.T, r rig, status int, provider string) {
		t.Helper()
		summaries, err := requestlog.QuerySummaries(r.dir, requestlog.Filter{})
		if err != nil {
			t.Fatalf("query summaries: %v", err)
		}
		if len(summaries) != 1 {
			t.Fatalf("list rows = %+v, want exactly the answered row (leg folded)", summaries)
		}
		row := summaries[0]
		if row.AttemptOnly || row.Status != status || row.Provider != provider {
			t.Errorf("list row = attempt_only:%v status:%d provider:%q, want status %d from %q", row.AttemptOnly, row.Status, row.Provider, status, provider)
		}
		wantHops := []requestlog.FailedHop{{Provider: "a", Status: http.StatusBadRequest, Attempt: 0}}
		if len(row.FailedHops) != len(wantHops) || row.FailedHops[0].Provider != wantHops[0].Provider || row.FailedHops[0].Status != wantHops[0].Status || row.FailedHops[0].Attempt != wantHops[0].Attempt {
			t.Errorf("failed_hops = %+v, want the lapsed account's 400 %+v", row.FailedHops, wantHops)
		}
		var legs int
		for _, record := range allRecords(t, r.dir) {
			if !record.AttemptOnly {
				continue
			}
			legs++
			if record.Provider != "a" || record.Status != http.StatusBadRequest || record.ResponseBody != lapsed {
				t.Errorf("leg record = provider %q status %d body %q, want the lapsed account's 400 and its upstream error body", record.Provider, record.Status, record.ResponseBody)
			}
		}
		if legs != 1 {
			t.Errorf("leg records = %d, want 1", legs)
		}
	}

	t.Run("lapsed account fails over to the next target", func(t *testing.T) {
		r := newRig(t, false, lapsedResponder, staticResponder(http.StatusOK, fallback))
		code, body := r.send()
		if code != http.StatusOK || !strings.Contains(body, "fallback") {
			t.Fatalf("status = %d body = %s, want the next target's 200", code, body)
		}
		if r.ups[0].hits() != 1 || r.ups[1].hits() != 1 {
			t.Errorf("hits = %d/%d, want 1/1", r.ups[0].hits(), r.ups[1].hits())
		}
		assertAccountFact(t, r, 0, 1)
		// The answered row is completed at response-body Close inside the
		// post-copy commit path — a Content-Length client can finish reading
		// first, and draining before that drops the row (the leg, written
		// synchronously in the forward loop, would then stand alone). Wait for
		// the commit metrics (recorded right after) so drain flushes it.
		awaitCommitMetrics(t, r.proxy, counters.PMKey{Provider: "b", Model: "m1"})
		r.drain()
		assertLogEvidence(t, r, http.StatusOK, "b")
	})

	t.Run("last candidate ends in the proxy 502", func(t *testing.T) {
		r := newRig(t, false, lapsedResponder)
		code, body := r.send()
		if code != http.StatusBadGateway || !strings.Contains(body, "all targets failed") {
			t.Fatalf("status = %d body = %q, want the 502 terminal (the account error is not passed through)", code, body)
		}
		assertAccountFact(t, r, 0, 1)
		r.drain()
		assertLogEvidence(t, r, http.StatusBadGateway, "(failover)")
	})

	// The incident's own shape: a load-balanced pool of three accounts, the
	// middle one lapsed. Rotation keeps landing requests on it; none of them
	// may absorb its 400.
	t.Run("no request in a rotating pool absorbs the account's 400", func(t *testing.T) {
		ok := staticResponder(http.StatusOK, fallback)
		r := newRig(t, true, ok, lapsedResponder, ok)
		for i := 0; i < 3; i++ {
			code, body := r.send()
			if code != http.StatusOK || !strings.Contains(body, "fallback") {
				t.Fatalf("request %d: status = %d body = %s, want a sibling's 200", i+1, code, body)
			}
		}
		if hits := r.ups[1].hits(); hits == 0 {
			t.Fatal("rotation never reached the lapsed account, so the scenario was not exercised")
		}
		assertAccountFact(t, r, 1, r.ups[1].hits())
	})
}

// TestForward_CredentialDenialSurfacesInStatusE2E pins the second source of
// the credential surfaces end to end through the real handler (contract:
// docs/web-api.md `credential_alerts` / `quota[name].denial`, docs/frontend.md
// 「全局凭据横幅」). The quota poll never sees these failures — step-plan's
// Quota() succeeds while its plan subscription is lapsed, and a console-only
// key can be revoked without the poll noticing — so the account attributions
// the executor proves must reach /api/status on their own: the global
// banner's alert list, the Accounts pill's per-account `denial`, and the
// schedule node's credential badge (which explains why the node keeps failing
// over). All three clear on the account's next 2xx.
func TestForward_CredentialDenialSurfacesInStatusE2E(t *testing.T) {
	const (
		lapsed   = `{"error":{"message":"you have no active step plan subscription","type":"request_params_invalid"}}`
		fallback = `{"choices":[{"message":{"role":"assistant","content":"fallback"}}]}`
		request  = `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`
	)
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass string
	}{
		{name: "lapsed plan subscription", status: http.StatusBadRequest, body: lapsed, wantClass: "plan-inactive"},
		{name: "revoked key", status: http.StatusUnauthorized, body: `{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`, wantClass: "auth"},
		{name: "account disabled", status: http.StatusForbidden, body: `{"error":{"code":"account_disabled"}}`, wantClass: "account-disabled"},
	}
	type alert struct {
		Key   string `json:"key"`
		Class string `json:"class"`
		Err   string `json:"err"`
		Since int64  `json:"since"`
	}
	// statusPayload is the slice of /api/status the credential surfaces read.
	type statusPayload struct {
		CredentialAlerts []alert `json:"credential_alerts"`
		Quota            map[string]struct {
			Denial *alert `json:"denial"`
		} `json:"quota"`
		Schedule struct {
			Models map[string]struct {
				Ordered []struct {
					Provider   string `json:"provider"`
					Credential string `json:"credential"`
				} `json:"ordered"`
			} `json:"models"`
		} `json:"schedule"`
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var denied atomic.Bool
			denied.Store(true)
			a := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if !denied.Load() {
					staticResponder(http.StatusOK, fallback)(w, r)
					return
				}
				w.Header().Set("content-type", "text/plain; charset=utf-8")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			b := newFakeUpstream(t, staticResponder(http.StatusOK, fallback))
			cfg := &configdomain.Config{
				Providers: map[string]configdomain.Provider{
					"a": {OpenAIBaseURL: a.srv.URL, Provider: testProviderID},
					"b": {OpenAIBaseURL: b.srv.URL, Provider: testProviderID},
				},
				Routes: map[string][]configdomain.RouteTarget{"m1": {
					{Provider: "a", Model: "m1", Priority: 1},
					{Provider: "b", Model: "m1", Priority: 2},
				}},
			}
			proxy, _, _ := newReqLogProxy(t, cfg)
			proxy.providers["a"] = &testProv{key: "a"}
			proxy.providers["b"] = &testProv{key: "b"}
			for _, name := range []string{"a", "b"} {
				proxy.runtimeState.SetQuota(name, &provider.QuotaSnapshot{Billing: provider.BillingUnknown}, 1)
			}
			server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
			t.Cleanup(server.Close)
			// x-mp-sticky: off keeps every request on the plain priority order,
			// so the recovery request is guaranteed to reach the account.
			send := func() int {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(request))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("x-mp-sticky", "off")
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if _, err := io.ReadAll(resp.Body); err != nil {
					t.Fatal(err)
				}
				return resp.StatusCode
			}
			// The status projection through the proxy's REAL admin ports (the
			// web layer only maps this DTO onto the wire).
			service := admin.New(proxy.adminPorts(func() string { return "" }, nil, nil, nil))
			status := func() statusPayload {
				t.Helper()
				dashboard := service.Dashboard(time.Now())
				payload := statusPayload{CredentialAlerts: make([]alert, 0, len(dashboard.CredentialAlerts))}
				for _, a := range dashboard.CredentialAlerts {
					payload.CredentialAlerts = append(payload.CredentialAlerts, alert{Key: a.Key, Class: a.Class, Err: a.Err, Since: a.Since})
				}
				quotaJSON, err := json.Marshal(dashboard.Quota)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(quotaJSON, &payload.Quota); err != nil {
					t.Fatalf("quota projection: %v\n%s", err, quotaJSON)
				}
				if err := json.Unmarshal(dashboard.Schedule, &payload.Schedule); err != nil {
					t.Fatalf("schedule projection: %v\n%s", err, dashboard.Schedule)
				}
				return payload
			}
			badges := func(payload statusPayload) map[string]string {
				out := map[string]string{}
				for _, node := range payload.Schedule.Models["m1"].Ordered {
					out[node.Provider] = node.Credential
				}
				return out
			}

			if code := send(); code != http.StatusOK {
				t.Fatalf("client status = %d, want the next target's 200", code)
			}
			got := status()
			if len(got.CredentialAlerts) != 1 || got.CredentialAlerts[0].Key != "a" || got.CredentialAlerts[0].Class != tc.wantClass ||
				got.CredentialAlerts[0].Err != tc.body || got.CredentialAlerts[0].Since == 0 {
				t.Fatalf("credential_alerts = %+v, want exactly one entry for a: class %q, the upstream evidence, a since", got.CredentialAlerts, tc.wantClass)
			}
			if d := got.Quota["a"].Denial; d == nil || d.Class != tc.wantClass || d.Err != tc.body || d.Since == 0 {
				t.Errorf("quota[a].denial = %+v, want class %q with the upstream evidence", d, tc.wantClass)
			}
			if d := got.Quota["b"].Denial; d != nil {
				t.Errorf("quota[b].denial = %+v, want none for the healthy sibling", d)
			}
			if gotBadges := badges(got); gotBadges["a"] != tc.wantClass || gotBadges["b"] != "" {
				t.Errorf("schedule credential badges = %+v, want %q on a only", gotBadges, tc.wantClass)
			}

			// Recovery: the account's next 2xx proves the credential and
			// clears every surface.
			denied.Store(false)
			if code := send(); code != http.StatusOK {
				t.Fatalf("client status after recovery = %d, want 200", code)
			}
			got = status()
			if gotBadges := badges(got); len(got.CredentialAlerts) != 0 || got.Quota["a"].Denial != nil || gotBadges["a"] != "" {
				t.Fatalf("surfaces after recovery = alerts %+v / denial %+v / badges %+v, want all clear", got.CredentialAlerts, got.Quota["a"].Denial, gotBadges)
			}
		})
	}
}
