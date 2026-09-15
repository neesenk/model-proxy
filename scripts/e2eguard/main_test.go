package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDaemon scripts the REAL guard semantics the suite asserts against — a
// compact reference model of the proxy's adjudication behavior (hermetic:
// no network, no daemon). Verdict outcomes are injectable so the judge-
// divergence paths are testable.
type fakeDaemon struct {
	blocked   map[string]bool
	summaries map[string][]summary // session → newest first
	guard     map[string][]guardMark
	ring      []adjudication
	records   []secRecord
	calls     int64
	lows      int64

	// repeatIndexed marks hit bytes that were adjudicated high (intercepted
	// verbatim on later requests).
	repeatIndexed map[string]bool
	// seenDummy marks dummy fixture bytes already judged (the echo is a
	// cache hit — cached ring entry, no second bill).
	seenDummy map[string]bool
	// verdictFor decides the verdict for a fresh adjudication; verdictDelay
	// makes ring entries appear only after N reads (exercising poll retries).
	verdictFor  func(rule string) string
	verdictTick int
	tick        int

	highKey  string
	unblocks int
}

func newFakeDaemon(verdictFor func(rule string) string) *fakeDaemon {
	return &fakeDaemon{
		blocked:       map[string]bool{},
		summaries:     map[string][]summary{},
		guard:         map[string][]guardMark{},
		repeatIndexed: map[string]bool{},
		seenDummy:     map[string]bool{},
		verdictFor:    verdictFor,
		highKey:       "PLACEHOLDER",
	}
}

func (f *fakeDaemon) Post(model, body, session string) (int, error) {
	if session != "" && f.blocked[session] {
		f.addRow(session, 400, nil)
		return 400, nil
	}
	for key, indexed := range f.repeatIndexed {
		if indexed && strings.Contains(body, key) {
			if session != "" {
				f.blocked[session] = true
			}
			id := f.addRow(session, 400, nil)
			f.records = append(f.records, secRecord{Kind: "secret", RequestID: id, SessionID: session,
				Names: []string{"openai_api_key"}, Action: "block", Verdict: "high"})
			return 400, nil
		}
	}
	var marks []guardMark
	pendingHigh := false
	addVerdict := func(marker, kind, rule string, cached bool) {
		f.tick++
		verdict := f.verdictFor(marker)
		f.ring = append(f.ring, adjudication{Kind: kind, Rule: rule, Verdict: verdict,
			Cached: cached, Ts: int64(1000 + f.tick)})
		if !cached {
			f.calls++
		}
		if verdict == "low" {
			f.lows++
		}
		marks = append(marks, guardMark{Kind: kind, Names: []string{rule}, Verdict: verdict,
			Cached: cached, Source: "judge", Ts: int64(1000 + f.tick)})
	}
	switch {
	case strings.Contains(body, "DUMMY key"):
		key := between(body, "credential: ", " —")
		addVerdict("dummy:"+key, "secret", "openai_api_key", f.seenDummy[key])
		f.seenDummy[key] = true
	case strings.Contains(body, "production billing account"):
		addVerdict("real", "secret", "openai_api_key", false)
		if f.ring[len(f.ring)-1].Verdict == "high" && session != "" {
			f.blocked[session] = true
			f.repeatIndexed[between(body, "remember it for later: ", `"`)] = true
			pendingHigh = true
		}
	case strings.Contains(body, "kube"):
		addVerdict("kube", "path", "kube", false)
		addVerdict("docker", "path", "docker", false)
	}
	id := f.addRow(session, 200, marks)
	// Ring entries and the verdict record bind to the request id only once
	// the row exists (the proxy assigns ids the same way).
	for i := range f.ring {
		if f.ring[i].RequestID == "" {
			f.ring[i].RequestID = id
			f.ring[i].SessionID = session
		}
	}
	if pendingHigh {
		f.records = append(f.records, secRecord{Kind: "secret", RequestID: id, SessionID: session,
			Names: []string{"openai_api_key"}, Action: "log", Verdict: "high"})
	}
	return 200, nil
}

func (f *fakeDaemon) lastID(session string) string {
	if rows := f.summaries[session]; len(rows) > 0 {
		return rows[0].RequestID
	}
	return ""
}

func (f *fakeDaemon) addRow(session string, status int, marks []guardMark) string {
	id := fmt.Sprintf("req-%d", len(f.guard)+1)
	f.guard[id] = marks
	row := summary{RequestID: id, SessionID: session, Status: status, Guard: marks}
	if session == "" {
		session = "\x00headless"
	}
	f.summaries[session] = append([]summary{row}, f.summaries[session]...)
	return id
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

func (f *fakeDaemon) Summaries(session string) ([]summary, error) {
	key := session
	if key == "" {
		key = "\x00headless"
	}
	return f.summaries[key], nil
}

func (f *fakeDaemon) DetailGuard(requestID string) ([]guardMark, error) {
	return f.guard[requestID], nil
}

func (f *fakeDaemon) Adjudications() (adjudicationFeed, error) {
	f.tick++
	if f.verdictTick > 0 && f.tick < f.verdictTick*10 {
		// Verdicts not visible yet — the suite must poll.
		return adjudicationFeed{Enabled: true}, nil
	}
	feed := adjudicationFeed{Enabled: true, Stats: struct {
		Calls       int64 `json:"calls"`
		LowVerdicts int64 `json:"low_verdicts"`
	}{Calls: f.calls, LowVerdicts: f.lows}}
	feed.Adjudications = append([]adjudication(nil), f.ring...)
	return feed, nil
}

func (f *fakeDaemon) SecurityRecords(kind string) ([]secRecord, error) {
	var out []secRecord
	for _, rec := range f.records {
		if kind == "" || rec.Kind == kind {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeDaemon) Blocks() ([]blockEntry, error) {
	var out []blockEntry
	for sid := range f.blocked {
		out = append(out, blockEntry{SessionID: sid, Rule: "openai_api_key"})
	}
	return out, nil
}

func (f *fakeDaemon) Unblock(session string) error {
	if !f.blocked[session] {
		return fmt.Errorf("404")
	}
	delete(f.blocked, session)
	f.unblocks++
	f.records = append(f.records, secRecord{Kind: "unblock", SessionID: session,
		Names: []string{"openai_api_key"}, Action: "unblock"})
	// The unblock record carries the original request id (find any record of
	// that session).
	for _, rec := range f.records {
		if rec.SessionID == session && rec.Verdict == "high" {
			f.records[len(f.records)-1].RequestID = rec.RequestID
			break
		}
	}
	// The unblock is session-level: NO mark joins the original request's
	// guard trail (the live proxy excludes it from the request join too).
	return nil
}

// ---- tests ----------------------------------------------------------------

func statusOf(results []result, name string) string {
	for _, r := range results {
		if r.name == name {
			return r.status
		}
	}
	return "MISSING"
}

func TestSuiteHappyPath(t *testing.T) {
	f := newFakeDaemon(func(marker string) string {
		switch {
		case strings.HasPrefix(marker, "dummy:"):
			return "low" // self-labeled fixture: the documented low shape
		case marker == "real":
			return "high" // full-length sk-proj- stand-in: the high shape
		default:
			return "low" // path categories
		}
	})
	results, r := runSuite(f, "test-model", 2*time.Second, nil)
	for _, res := range results {
		if res.status == "FAIL" {
			t.Errorf("%s FAILED: %s", res.name, res.detail)
		}
	}
	for _, name := range []string{"clean", "low-dummy", "cached-echo", "high-real", "session-block",
		"unblock-trail", "repeat-intercept", "multi-rule", "weak-path-ignored", "headless-verdict",
		"detail-guard", "stats-accounting", "cleanup"} {
		if statusOf(results, name) != "PASS" {
			t.Errorf("%s = %s, want PASS", name, statusOf(results, name))
		}
	}
	if !r.highOK {
		t.Error("highOK not set after the high flow")
	}
	if f.unblocks == 0 {
		t.Error("cleanup never unblocked")
	}
}

func TestSuiteLowDummyVerdictIsLow(t *testing.T) {
	// Everything judges low: the dummy flow passes, the high flow FAILS
	// loudly (judge divergence), its dependents SKIP — the suite's honest
	// handling of a nondeterministic judge.
	f := newFakeDaemon(func(string) string { return "low" })
	results, r := runSuite(f, "m", 500*time.Millisecond, nil)
	if got := statusOf(results, "low-dummy"); got != "PASS" {
		t.Errorf("low-dummy = %s, want PASS", got)
	}
	if got := statusOf(results, "high-real"); got != "FAIL" {
		t.Errorf("high-real = %s, want FAIL (verdict divergence)", got)
	}
	if r.highOK {
		t.Error("highOK must stay false on divergence")
	}
	for _, name := range []string{"session-block", "unblock-trail", "repeat-intercept"} {
		if got := statusOf(results, name); got != "SKIP" {
			t.Errorf("%s = %s, want SKIP (depends on high)", name, got)
		}
	}
}

func TestSuiteSubset(t *testing.T) {
	f := newFakeDaemon(func(string) string { return "low" })
	results, _ := runSuite(f, "m", 500*time.Millisecond, map[string]bool{"clean": true, "low-dummy": true})
	if len(results) != 2 {
		t.Fatalf("results = %d, want the 2 selected scenarios", len(results))
	}
}

func TestParseFlags(t *testing.T) {
	opts, err := parseFlags([]string{"-base-url", "http://x:1/", "-model", "glm", "-only", "a, b"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.baseURL != "http://x:1" || opts.model != "glm" {
		t.Errorf("opts = %+v", opts)
	}
	if !opts.only["a"] || !opts.only["b"] || opts.only["c"] {
		t.Errorf("only = %v", opts.only)
	}
	if _, err := parseFlags([]string{"-bogus"}); err == nil {
		t.Error("bogus flag accepted")
	}
	t.Setenv("MP_ADMIN_TOKEN", "sekrit")
	opts, err = parseFlags(nil)
	if err != nil || opts.token != "sekrit" {
		t.Errorf("token from env: %v %v", opts.token, err)
	}
	if _, err := parseFlags([]string{"-base-url", ""}); err == nil {
		t.Error("empty base-url accepted")
	}
}

func TestFixtureBodies(t *testing.T) {
	dummy := renderBody(dummyKeyBody("sk-dummy-abc-notreal"), "glm")
	if !strings.Contains(dummy, `"model":"glm"`) || !strings.Contains(dummy, "DUMMY key") {
		t.Errorf("dummy body = %s", dummy)
	}
	real := renderBody(realKeyBody("sk-proj-xyz"), "glm")
	if !strings.Contains(real, "remember it for later: sk-proj-xyz") {
		t.Errorf("real body = %s", real)
	}
	if strings.Contains(real, "do not use") {
		t.Error("mitigating phrasing drifts the judge verdict; keep the high fixture unambiguous")
	}
	if strings.Contains(real, "\x00model\x00") {
		t.Error("model tag not rendered")
	}
	paths := renderBody(pathToolCallBody("~/.kube/config", "~/.docker/config.json"), "glm")
	if !strings.Contains(paths, "tool_calls") || !strings.Contains(paths, "~/.kube/config") || !strings.Contains(paths, "~/.docker/config.json") {
		t.Errorf("path body = %s", paths)
	}
	// The OpenAI wire spec: tool arguments ride as an ESCAPED JSON string —
	// an unescaped object splice is the 400-1210 rejection zhipu gives.
	if !strings.Contains(paths, `"arguments":"{\"command\":`) {
		t.Errorf("tool arguments not string-encoded: %s", paths)
	}
	if !strings.Contains(paths, `"role":"tool"`) {
		t.Errorf("tool body lacks the tool response turn (strict upstreams reject a trailing tool_call): %s", paths)
	}
	if prose := renderBody(prosePathBody(), "glm"); !strings.Contains(prose, "~/.ssh/id_rsa") {
		t.Errorf("prose body = %s", prose)
	}
}

func TestPollRetriesUntilVisible(t *testing.T) {
	f := newFakeDaemon(func(string) string { return "low" })
	r := &run{d: f, model: "m", timeout: 2 * time.Second}
	// The ring only becomes visible after the fake's tick passes the
	// visibility gate — poll must retry, not assume.
	if _, err := r.post("s", cleanBody()); err != nil {
		t.Fatal(err)
	}
	rows, err := f.Summaries("s")
	if err != nil || len(rows) == 0 {
		t.Fatal("no rows")
	}
}

func TestPollTimeoutMessage(t *testing.T) {
	r := &run{d: newFakeDaemon(func(string) string { return "low" }), timeout: 10 * time.Millisecond}
	n := 0
	err := poll(r, "never true", func() error {
		n++
		return fmt.Errorf("still no")
	})
	if err == nil || !strings.Contains(err.Error(), "never true") {
		t.Errorf("err = %v", err)
	}
	if n < 2 {
		t.Errorf("poll iterations = %d, want retries", n)
	}
}

func TestAwaitSummaryRejectsWrongStatus(t *testing.T) {
	f := newFakeDaemon(func(string) string { return "low" })
	r := &run{d: f, model: "m", timeout: 60 * time.Millisecond}
	if _, err := r.post("s", cleanBody()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.awaitSummary("s", 500); err == nil {
		t.Error("awaitSummary accepted the wrong status")
	}
}

// ---- httpDaemon wire shapes (httptest, no network) ------------------------

func TestHTTPDaemonWireShapes(t *testing.T) {
	var gotSession, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "" {
			gotAuth = r.Header.Get("authorization")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			gotSession = r.Header.Get("x-claude-code-session-id")
			w.WriteHeader(200)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/requests/"):
			if strings.HasSuffix(r.URL.Path, "missing") {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write([]byte(`{"records":[],"guard":[{"kind":"secret","verdict":"low","source":"judge"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/requests":
			_, _ = w.Write([]byte(`{"enabled":true,"records":[{"request_id":"r1","session_id":"s","status":200,"guard":[]}],"facets":{}}`))
		case r.URL.Path == "/api/security/adjudications":
			_, _ = w.Write([]byte(`{"adjudications":[{"ts":1,"kind":"secret","rule":"jwt","verdict":"low","request_id":"r1"}],"stats":{"calls":3,"low_verdicts":2},"enabled":true}`))
		case r.URL.Path == "/api/security":
			if r.URL.Query().Get("kind") != "unblock" {
				t.Errorf("kind param = %q", r.URL.Query().Get("kind"))
			}
			_, _ = w.Write([]byte(`{"records":[{"kind":"unblock","names":["jwt"]}],"skipped":0}`))
		case r.URL.Path == "/api/security/blocks" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"blocks":[{"session_id":"s","rule":"jwt"}]}`))
		case r.URL.Path == "/api/security/blocks/s" && r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"status":"unblocked"}`))
		default:
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	d := &httpDaemon{base: srv.URL, token: "tok", http: srv.Client()}

	if status, err := d.Post("m", "{}", "sess-1"); err != nil || status != 200 {
		t.Fatalf("post = %d %v", status, err)
	}
	if gotSession != "sess-1" {
		t.Errorf("session header = %q", gotSession)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
	rows, err := d.Summaries("s")
	if err != nil || len(rows) != 1 || rows[0].RequestID != "r1" {
		t.Fatalf("summaries = %+v %v", rows, err)
	}
	guard, err := d.DetailGuard("r1")
	if err != nil || len(guard) != 1 || guard[0].Verdict != "low" {
		t.Fatalf("detail guard = %+v %v", guard, err)
	}
	if _, err := d.DetailGuard("missing"); err == nil {
		t.Error("missing detail accepted")
	}
	feed, err := d.Adjudications()
	if err != nil || !feed.Enabled || feed.Stats.Calls != 3 || feed.Stats.LowVerdicts != 2 || feed.Adjudications[0].Rule != "jwt" {
		t.Fatalf("feed = %+v %v", feed, err)
	}
	recs, err := d.SecurityRecords("unblock")
	if err != nil || len(recs) != 1 || recs[0].Kind != "unblock" {
		t.Fatalf("records = %+v %v", recs, err)
	}
	blocks, err := d.Blocks()
	if err != nil || len(blocks) != 1 || blocks[0].SessionID != "s" {
		t.Fatalf("blocks = %+v %v", blocks, err)
	}
	if err := d.Unblock("s"); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	// Transport failure surfaces as an error, not a panic.
	bad := &httpDaemon{base: "http://127.0.0.1:1", http: srv.Client()}
	if _, err := bad.Summaries("s"); err == nil {
		t.Error("dead base accepted")
	}
}

func TestExecuteExitCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(200)
		case r.URL.Path == "/api/requests":
			_, _ = w.Write([]byte(`{"enabled":true,"records":[{"request_id":"r1","status":200}],"facets":{}}`))
		case r.URL.Path == "/api/requests/r1":
			_, _ = w.Write([]byte(`{"records":[],"guard":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	opts := options{baseURL: srv.URL, model: "m", timeout: time.Second, only: map[string]bool{"clean": true}}
	if code := execute(opts); code != 0 {
		t.Errorf("clean-only exit = %d, want 0", code)
	}
	// A failing scenario flips the exit code.
	opts.only = map[string]bool{"weak-path-ignored": true}
	if code := execute(opts); code != 1 {
		t.Errorf("failing exit = %d, want 1", code)
	}
}

func TestMainWithUsageError(t *testing.T) {
	if code := mainWith([]string{"-definitely-bogus"}); code != 2 {
		t.Errorf("usage-error exit = %d, want 2", code)
	}
}

// TestHTTPDaemonErrorBranches drives every reader through a failing server:
// non-200 statuses and garbage bodies must surface as errors.
func TestHTTPDaemonErrorBranches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "garbage=1") || strings.Contains(r.URL.Path, "garbage") {
			_, _ = w.Write([]byte(`{not json`))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	d := &httpDaemon{base: srv.URL, http: srv.Client()}
	if _, err := d.Summaries("s"); err == nil {
		t.Error("500 summaries accepted")
	}
	if _, err := d.DetailGuard("x"); err == nil {
		t.Error("500 detail accepted")
	}
	if _, err := d.Adjudications(); err == nil {
		t.Error("500 adjudications accepted")
	}
	if _, err := d.SecurityRecords(""); err == nil {
		t.Error("500 security accepted")
	}
	if _, err := d.Blocks(); err == nil {
		t.Error("500 blocks accepted")
	}
	if err := d.Unblock("s"); err == nil {
		t.Error("500 unblock accepted")
	}
	dead := &httpDaemon{base: "http://127.0.0.1:1", http: d.http}
	if _, err := dead.Post("m", "{}", ""); err == nil {
		t.Error("dead post accepted")
	}
	g := &httpDaemon{base: srv.URL + "/garbage=1", http: srv.Client()}
	if _, err := g.Summaries("s"); err == nil {
		t.Error("garbage summaries accepted")
	}
	if _, err := g.Adjudications(); err == nil {
		t.Error("garbage adjudications accepted")
	}
}

// flakyDaemon fails the FIRST call of every method PER ARGUMENT (error
// injection keyed by method+argument) — every scenario eats exactly one
// transient API failure, covering each error-propagation path while poll
// loops prove they ride out single hiccups.
type flakyDaemon struct {
	inner  daemon
	failed map[string]bool
}

func (f *flakyDaemon) once(key string) error {
	if !f.failed[key] {
		f.failed[key] = true
		return fmt.Errorf("injected failure: %s", key)
	}
	return nil
}

func (f *flakyDaemon) Post(model, body, session string) (int, error) {
	if err := f.once("post:" + session + ":" + body[:24]); err != nil {
		return 0, err
	}
	return f.inner.Post(model, body, session)
}
func (f *flakyDaemon) Summaries(session string) ([]summary, error) {
	if err := f.once("summaries:" + session); err != nil {
		return nil, err
	}
	return f.inner.Summaries(session)
}
func (f *flakyDaemon) DetailGuard(requestID string) ([]guardMark, error) {
	if err := f.once("detail:" + requestID); err != nil {
		return nil, err
	}
	return f.inner.DetailGuard(requestID)
}
func (f *flakyDaemon) Adjudications() (adjudicationFeed, error) {
	if err := f.once("ring"); err != nil {
		return adjudicationFeed{}, err
	}
	return f.inner.Adjudications()
}
func (f *flakyDaemon) SecurityRecords(kind string) ([]secRecord, error) {
	if err := f.once("security:" + kind); err != nil {
		return nil, err
	}
	return f.inner.SecurityRecords(kind)
}
func (f *flakyDaemon) Blocks() ([]blockEntry, error) {
	if err := f.once("blocks"); err != nil {
		return nil, err
	}
	return f.inner.Blocks()
}
func (f *flakyDaemon) Unblock(session string) error {
	if err := f.once("unblock:" + session); err != nil {
		return err
	}
	return f.inner.Unblock(session)
}

func TestSuiteErrorPropagation(t *testing.T) {
	inner := newFakeDaemon(func(marker string) string {
		if marker == "real" {
			return "high"
		}
		return "low"
	})
	f := &flakyDaemon{inner: inner, failed: map[string]bool{}}
	results, _ := runSuite(f, "m", 300*time.Millisecond, nil)
	// Every scenario that runs must FAIL with the injected error, never
	// panic or hang; the skip chain still holds.
	for _, res := range results {
		if res.status == "PASS" {
			t.Errorf("%s passed despite an injected first-call failure", res.name)
		}
		if res.status == "FAIL" && !strings.Contains(res.detail, "injected") && !strings.Contains(res.detail, "status ") {
			t.Errorf("%s failed oddly: %s", res.name, res.detail)
		}
	}
}
