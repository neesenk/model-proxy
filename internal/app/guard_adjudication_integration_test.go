package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/observe/seclog"
)

// End-to-end wiring of the guard AI second-opinion channel: a pattern-table
// hit defers to the designated model (provider-direct, judge httptest
// upstream), the verdict flows back through the sink (audit + counters), a
// high verdict blocks the session, and the admin unblock port re-admits it.
// All fixtures are synthetic (AGENTS.md credential red line).

// judgeUpstream is a fake /v1/messages adjudication model: it answers with
// the scripted verdict JSON and records the prompts it was asked.
type judgeUpstream struct {
	mu      sync.Mutex
	prompts []string
	verdict string // "high" | "low" | "" (model breaks → fail-open)
	reason  string
	srv     *httptest.Server
}

func newJudgeUpstream(t *testing.T, verdict, reason string) *judgeUpstream {
	t.Helper()
	j := &judgeUpstream{verdict: verdict, reason: reason}
	j.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		j.mu.Lock()
		j.prompts = append(j.prompts, string(b))
		j.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		if j.verdict == "" {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"model unavailable"}`)
			return
		}
		inner, _ := json.Marshal(map[string]string{"risk": j.verdict, "reason": j.reason})
		reply, _ := json.Marshal(map[string]any{
			"content": []map[string]string{{"type": "text", "text": string(inner)}},
		})
		w.Write(reply)
	}))
	t.Cleanup(j.srv.Close)
	return j
}

func (j *judgeUpstream) calls() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.prompts)
}

// newAdjudicationProxy wires: main upstream (chat leg) + judge provider
// (anthropic leg) + guard.adjudicate on with block_session. Returns the
// proxy, its URL, the judge, the main-upstream body log, the isolated home
// dir (seclog/pool state) and the isolated state dir (quota/adjudication
// state, for asserting persisted guard files).
func newAdjudicationProxy(t *testing.T, judge *judgeUpstream, paths string) (p *Proxy, proxyURL string, bodies func() []string, home, stateDir string) {
	t.Helper()
	home = t.TempDir()
	setPoolHome(t, home)

	var mu sync.Mutex
	var ups []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		ups = append(ups, string(b))
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)

	cfg := &Config{
		Providers: map[string]Provider{
			"static": {OpenAIBaseURL: up.URL, Provider: testProviderID},
			"judge":  {AnthropicBaseURL: judge.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"glm":   {{Provider: "static", Model: "glm"}},
			"judge": {{Provider: "judge", Model: "judge-model"}},
		},
		Guard: GuardConfig{Secrets: "log", Paths: paths, Audit: true,
			Adjudicate: GuardAdjudicateConfig{Enabled: true, Model: "judge", BlockSession: true}},
	}
	// The guard state files (verdict cache, blocks) live next to the injected
	// quota state path (CacheStatePath recipe), NOT under HOME.
	stateDir = t.TempDir()
	p = newProxyWithStaticAt(t, cfg, filepath.Join(stateDir, "quota_state.json"), map[string]string{"static": "k", "judge": "jk"})
	// Boot reconcile (StartRuntimeServices does this in production): brings
	// the security audit logger up so verdict-side records persist.
	p.reconcileSecLog(cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)
	return p, px.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ups...)
	}, home, stateDir
}

const adjudDummyKey = "sk-inttest-dummy-not-a-real-key"

func postWithSession(t *testing.T, url, body, session string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if session != "" {
		req.Header.Set("x-claude-code-session-id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// The production regression this feature exists for: a dummy/fixture key
// shape is adjudicated LOW → suppressed (no audit record, no block), the
// verdict is cached so history echo does not re-bill, and the request still
// flows upstream.
func TestGuardAdjudication_LowVerdictSuppressesDummyKey(t *testing.T) {
	judge := newJudgeUpstream(t, "low", "fixture text in code")
	p, proxyURL, bodies, home, stateDir := newAdjudicationProxy(t, judge, "off")

	resp := postWithSession(t, proxyURL, guardPoolRequestBody(adjudDummyKey), "sess-low")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(bodies()) != 1 {
		t.Fatalf("upstream calls = %d, want 1 (deferred channel never blocks sync)", len(bodies()))
	}
	waitForAdjudication(t, judge, 1)

	// History echo: the same content re-requested is served from the verdict
	// cache — the judge is NOT called again.
	resp = postWithSession(t, proxyURL, guardPoolRequestBody(adjudDummyKey), "sess-low")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("echo status = %d, want 200", resp.StatusCode)
	}
	waitForAdjudication(t, judge, 1)
	if judge.calls() != 1 {
		t.Fatalf("judge calls = %d, want 1 (cache dedup)", judge.calls())
	}

	// No guard event for the suppressed pattern hit, no audit record, no block.
	for _, d := range guardEventDetails(p) {
		if strings.Contains(d, "openai_api_key") {
			t.Errorf("suppressed hit emitted an immediate event: %q", d)
		}
	}
	if _, ok := p.adjudication.Blocked("sess-low"); ok {
		t.Error("low verdict must never block")
	}
	// The verdict is RECORDED once per unique content (verdict=low is the
	// suppression marker in the durable audit trail) — and the cached echo
	// must not have written a second record.
	records := readSeclogRecords(t, home)
	lowRecords := 0
	for _, r := range records {
		if r.Verdict == "low" {
			lowRecords++
			if len(r.Names) != 1 || r.Names[0] != "openai_api_key" {
				t.Errorf("low record names = %v", r.Names)
			}
			if strings.Contains(r.Detail, adjudDummyKey) {
				t.Error("low record leaked the matched bytes")
			}
		}
	}
	if lowRecords != 1 {
		t.Errorf("low verdict records = %d, want exactly 1 (cached echo must not re-record)", lowRecords)
	}
	// The verdict cache persisted (hash keys only — no fixture bytes).
	data, err := readFileIfExists(filepath.Join(stateDir, "guard_verdicts.json"))
	if err != nil || !strings.Contains(data, "\"low\"") {
		t.Errorf("verdict cache not persisted: %v", err)
	}
	if strings.Contains(data, adjudDummyKey) {
		t.Error("verdict cache persisted the matched bytes")
	}
}

// A high verdict records an audit record (verdict=high), blocks the session,
// and the block is enforced on the session's next request until the admin
// unblock port clears it.
func TestGuardAdjudication_HighVerdictBlocksAndUnblocks(t *testing.T) {
	judge := newJudgeUpstream(t, "high", "live key material")
	p, proxyURL, _, home, _ := newAdjudicationProxy(t, judge, "off")

	if resp := postWithSession(t, proxyURL, guardPoolRequestBody(adjudDummyKey), "sess-hi"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200 (async channel)", resp.StatusCode)
	}
	waitForAdjudication(t, judge, 1)

	// Blocked: the next request of the session 400s with the unblock hint
	// and never reaches the upstream.
	waitForBlock(t, p, "sess-hi")
	resp := postWithSession(t, proxyURL, guardPoolRequestBody("clean body"), "sess-hi")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("blocked status = %d, want 400", resp.StatusCode)
	}
	// Other sessions are unaffected.
	if resp := postWithSession(t, proxyURL, guardPoolRequestBody("clean body"), "sess-other"); resp.StatusCode != http.StatusOK {
		t.Fatalf("other session status = %d, want 200", resp.StatusCode)
	}

	// The high verdict landed in the audit log with the verdict field.
	records := readSeclogRecords(t, home)
	found := false
	for _, r := range records {
		if r.Verdict == "high" && r.Kind == seclog.KindSecret && len(r.Names) == 1 && r.Names[0] == "openai_api_key" {
			found = true
			if strings.Contains(r.Detail, adjudDummyKey) {
				t.Error("audit record leaked the matched bytes")
			}
		}
	}
	if !found {
		t.Errorf("no verdict=high audit record in %d records", len(records))
	}

	// Unblock via the admin port → the session flows again.
	if err := p.webUnblockForTest("sess-hi"); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if resp := postWithSession(t, proxyURL, guardPoolRequestBody("clean body"), "sess-hi"); resp.StatusCode != http.StatusOK {
		t.Fatalf("post-unblock status = %d, want 200", resp.StatusCode)
	}
	// The unblock persisted: a service rebuild over the same home is clean.
	if _, ok := p.adjudication.Blocked("sess-hi"); ok {
		t.Error("session still blocked after unblock")
	}
}

// A broken judge (5xx) fails OPEN: the classic immediate record reappears
// with verdict=error and nothing is blocked.
func TestGuardAdjudication_JudgeErrorFailsOpen(t *testing.T) {
	judge := newJudgeUpstream(t, "", "")
	p, proxyURL, _, home, _ := newAdjudicationProxy(t, judge, "off")

	if resp := postWithSession(t, proxyURL, guardPoolRequestBody(adjudDummyKey), "sess-err"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	waitForAdjudication(t, judge, 1)

	records := waitForAuditVerdict(t, home, "error")
	if len(records) == 0 {
		t.Fatal("fail-open did not write an audit record")
	}
	if _, ok := p.adjudication.Blocked("sess-err"); ok {
		t.Error("judge error must not block the session")
	}
}

// A HIGH verdict on a strong PATH hit must land in the audit log under
// kind=path (not secret) and block the session — the Security surfaces'
// kind filter keys off the audit record kind.
func TestGuardAdjudication_HighPathVerdictRecordsAsPathKind(t *testing.T) {
	judge := newJudgeUpstream(t, "high", "tool reads key file")
	p, proxyURL, _, home, _ := newAdjudicationProxy(t, judge, "log")

	body := `{"model":"glm","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"cat ~/.ssh/id_rsa"}}]}]}`
	if resp := postWithSession(t, proxyURL, body, "sess-path"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (deferred channel never blocks synchronously)", resp.StatusCode)
	}
	waitForAdjudication(t, judge, 1)
	waitForBlock(t, p, "sess-path")

	records := readSeclogRecords(t, home)
	found := false
	for _, r := range records {
		if r.Verdict == "high" && len(r.Names) == 1 && r.Names[0] == "ssh" {
			found = true
			if r.Kind != seclog.KindPath {
				t.Errorf("high path-verdict record kind = %q, want %q", r.Kind, seclog.KindPath)
			}
		}
	}
	if !found {
		t.Errorf("no verdict=high ssh record in %d records", len(records))
	}
}

// ---- helpers ----

func waitForAdjudication(t *testing.T, j *judgeUpstream, calls int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if j.calls() >= calls {
			// Small grace for the sink side effects to land after the call.
			time.Sleep(30 * time.Millisecond)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("judge not called (want %d)", calls)
}

func waitForBlock(t *testing.T, p *Proxy, session string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := p.adjudication.Blocked(session); ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("session not blocked after high verdict")
}

func waitForAuditVerdict(t *testing.T, home, verdict string) []*seclog.Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range readSeclogRecords(t, home) {
			if r.Verdict == verdict {
				return readSeclogRecords(t, home)
			}
		}
		time.Sleep(3 * time.Millisecond)
	}
	return nil
}

// webUnblockForTest drives the admin port path the CLI/WebUI use.
func (p *Proxy) webUnblockForTest(sessionID string) error {
	if !p.adjudicationUnblock(sessionID) {
		return fmt.Errorf("session %s not blocked", sessionID)
	}
	return nil
}

// readSeclogRecords reads the isolated home's security audit JSONL.
func readSeclogRecords(t *testing.T, home string) []*seclog.Record {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(home, ".model-proxy", "log", "security", "security-*.log"))
	var out []*seclog.Record
	for _, m := range matches {
		data, err := readFileIfExists(m)
		if err != nil {
			t.Fatalf("read seclog: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
			if line == "" {
				continue
			}
			var r seclog.Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				continue // torn tail line
			}
			out = append(out, &r)
		}
	}
	return out
}

func readFileIfExists(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// The production failover regression (the volcengine incident): the judge
// route's first target hangs and never answers. The scheduled exchange gives
// it only its own budget slice, fails over to the second judge provider, and
// the high verdict still blocks the session end to end — instead of the
// whole call budget starving on the hang and degrading to verdict=error.
func TestGuardAdjudication_FailoverPastHangingJudge(t *testing.T) {
	good := newJudgeUpstream(t, "high", "live key material")
	release := make(chan struct{})
	var hangCalls atomic.Int32
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hangCalls.Add(1)
		io.Copy(io.Discard, r.Body)
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) })

	home := t.TempDir()
	setPoolHome(t, home)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)

	cfg := &Config{
		Providers: map[string]Provider{
			"static":     {OpenAIBaseURL: up.URL, Provider: testProviderID},
			"judge-hang": {AnthropicBaseURL: hang.URL, Provider: "test-static"},
			"judge-good": {AnthropicBaseURL: good.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "static", Model: "glm"}},
			"judge": {
				{Provider: "judge-hang", Model: "judge-model", Priority: 1},
				{Provider: "judge-good", Model: "judge-model", Priority: 2},
			},
		},
		Guard: GuardConfig{Secrets: "log", Paths: "off", Audit: true,
			Adjudicate: GuardAdjudicateConfig{Enabled: true, Model: "judge", Timeout: "900ms", BlockSession: true}},
	}
	stateDir := t.TempDir()
	p := newProxyWithStaticAt(t, cfg, filepath.Join(stateDir, "quota_state.json"),
		map[string]string{"static": "k", "judge-hang": "kh", "judge-good": "kg"})
	p.reconcileSecLog(cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)

	start := time.Now()
	if resp := postWithSession(t, px.URL, guardPoolRequestBody(adjudDummyKey), "sess-fo"); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (async channel)", resp.StatusCode)
	}
	waitForAdjudication(t, good, 1)
	waitForBlock(t, p, "sess-fo")

	if good.calls() != 1 {
		t.Errorf("good judge calls = %d, want 1", good.calls())
	}
	// The judge prompt disables extended thinking: the reply is terse JSON,
	// and a thinking judge can outrun its attempt budget (15s vs 2s measured).
	if len(good.prompts) == 0 || !strings.Contains(good.prompts[0], `"thinking":{"type":"disabled"}`) {
		t.Error("judge prompt does not disable thinking")
	}
	if n := hangCalls.Load(); n != 1 {
		t.Errorf("hanging judge attempts = %d, want 1 (only its own budget slice)", n)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("verdict took %v — the hanging target must not consume the call budget", elapsed)
	}
	// The hang fed the shared breaker: the forward pipeline sees the same
	// provider-health view as the judge leg.
	st := p.runtimeState.Dashboard(time.Now()).Providers["judge-hang"]
	if st.ConsecutiveFailures == 0 {
		t.Error("hanging judge target left no failure trace in shared health state")
	}
}

// Repeat interception (decision: same mechanism as the exact-match channel):
// once content is adjudicated HIGH, any LATER request carrying the same hit
// bytes is rejected verbatim — synchronously, with no second judge call —
// and that request's session joins the block table. The index persists
// (guard_blocked.json, sha256 keys only), so a service rebuild still
// intercepts. This is the path a retrying or session-hopping client takes.
func TestGuardAdjudication_RepeatHighContentIntercepted(t *testing.T) {
	judge := newJudgeUpstream(t, "high", "live key material")
	p, proxyURL, bodies, home, stateDir := newAdjudicationProxy(t, judge, "off")

	// Seed: session A sends the leaked key once — forwarded (async channel),
	// judged high, A blocked, content indexed.
	if resp := postWithSession(t, proxyURL, guardPoolRequestBody(adjudDummyKey), "sess-src"); resp.StatusCode != http.StatusOK {
		t.Fatalf("seed status = %d, want 200", resp.StatusCode)
	}
	waitForAdjudication(t, judge, 1)
	waitForBlock(t, p, "sess-src")
	if len(bodies()) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(bodies()))
	}

	// Repeat: session B re-sends the SAME key — rejected with 400 immediately,
	// upstream never sees it, the judge is NOT called again, B is blocked.
	resp := postWithSession(t, proxyURL, guardPoolRequestBody(adjudDummyKey), "sess-b")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("repeat status = %d, want 400 (repeat of adjudicated-high content)", resp.StatusCode)
	}
	if len(bodies()) != 1 {
		t.Errorf("upstream calls after repeat = %d, want still 1 (intercepted before forward)", len(bodies()))
	}
	if judge.calls() != 1 {
		t.Errorf("judge calls after repeat = %d, want 1 (no second adjudication)", judge.calls())
	}
	waitForBlock(t, p, "sess-b")

	// The interception is attributed: an audit record with verdict=high and
	// action=block exists for the repeat (beyond the seeded verdict record).
	highBlock := 0
	for _, r := range readSeclogRecords(t, home) {
		if r.Verdict == "high" && r.Action == "block" {
			highBlock++
			if strings.Contains(r.Detail, adjudDummyKey) {
				t.Error("interception record leaked the matched bytes")
			}
		}
	}
	if highBlock == 0 {
		t.Error("no action=block verdict=high record for the intercepted repeat")
	}

	// The index persists hash-only: rebuild the service over the same state
	// dir and the repeat still intercepts (fresh proxy = restart semantics).
	data, err := readFileIfExists(filepath.Join(stateDir, "guard_blocked.json"))
	if err != nil {
		t.Fatalf("guard_blocked.json not written: %v", err)
	}
	if strings.Contains(data, adjudDummyKey) {
		t.Error("blocked-content index persisted the raw hit bytes")
	}

	// Session C, after a rebuild: still intercepted, still no judge call.
	resp = postWithSession(t, proxyURL, guardPoolRequestBody(adjudDummyKey), "sess-c")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("post-rebuild repeat status = %d, want 400", resp.StatusCode)
	}
	if judge.calls() != 1 {
		t.Errorf("judge calls after rebuild repeat = %d, want 1", judge.calls())
	}
	waitForBlock(t, p, "sess-c")
}
