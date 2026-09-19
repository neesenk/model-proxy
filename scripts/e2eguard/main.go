// Command e2eguard is the live end-to-end suite for the guard security
// surface: it drives REAL requests through a RUNNING model-proxy (real
// scheduling, real judge model via guard.adjudicate, real persistence —
// request log, security audit store, verdict cache, block table) and
// verifies every observable outcome through the same admin API the WebUI
// and CLI use. It is the live counterpart of the hermetic integration tests
// (internal/app/guard_adjudication_integration_test.go): those pin behavior
// against fakes, this pins the operator-visible reality of one deployment.
//
// It sends SYNTHETIC content only — shaped fixtures (self-labeled dummy
// keys, random per-run sk-proj- stand-ins, tool calls touching credential
// PATHS, never credential VALUES). The known-secret exact-match and
// cross-request fragmentation channels are deliberately NOT exercised here:
// driving them requires embedding a real configured credential in a request
// body, which would land it in the admin-only request log — hermetic tests
// cover both channels (TestServeGuardExactMatchIntercepts,
// TestServeGuardFragmentedSecretBlock).
//
// Usage:
//
//	go run ./scripts/e2eguard                                  # defaults below
//	go run ./scripts/e2eguard -base-url http://127.0.0.1:15722 -model glm-5.3-flash
//	go run ./scripts/e2eguard -only high-real,unblock          # subset
//	go run ./scripts/e2eguard -token "$(cat ~/.model-proxy/admin_token)"
//
// Requirements: guard.adjudicate enabled on the target daemon, request_log
// and guard.audit on (defaults), a working judge model route. Verdicts come
// from the real judge — scenario content is engineered for a stable verdict
// (self-labeled dummies → low, full-length sk-proj- stand-ins → high); when
// the judge disagrees, the scenario FAILS loudly rather than being assumed.
// Downstream scenarios of a non-high verdict SKIP with the reason (they are
// verdict-dependent by construction).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	os.Exit(mainWith(os.Args[1:]))
}

// mainWith is the testable main: parse, run, exit code (2 usage, 0/1 suite).
func mainWith(args []string) int {
	opts, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2eguard:", err)
		return 2
	}
	return execute(opts)
}

type options struct {
	baseURL string
	model   string
	token   string
	timeout time.Duration
	only    map[string]bool
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("e2eguard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	baseURL := fs.String("base-url", "http://127.0.0.1:15722", "running proxy base URL")
	model := fs.String("model", "glm-5.3-flash", "exposed route name requests call (also the judge target unless guard.adjudicate.model differs)")
	token := fs.String("token", "", "admin bearer token when admin auth is enabled (or set MP_ADMIN_TOKEN)")
	timeout := fs.Duration("timeout", 90*time.Second, "per-wait budget for async verdicts")
	only := fs.String("only", "", "comma-separated scenario subset (empty = all)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if *token == "" {
		*token = os.Getenv("MP_ADMIN_TOKEN")
	}
	opts := options{baseURL: strings.TrimRight(*baseURL, "/"), model: *model, token: *token, timeout: *timeout}
	if *only != "" {
		opts.only = map[string]bool{}
		for _, s := range strings.Split(*only, ",") {
			if s = strings.TrimSpace(s); s != "" {
				opts.only[s] = true
			}
		}
	}
	if opts.baseURL == "" {
		return options{}, fmt.Errorf("-base-url is required")
	}
	return opts, nil
}

// ---- daemon: the admin/proxy surface the scenarios drive -----------------

// summary mirrors the /api/requests record projection the suite asserts on.
type summary struct {
	RequestID string      `json:"request_id"`
	SessionID string      `json:"session_id"`
	Status    int         `json:"status"`
	Guard     []guardMark `json:"guard"`
}

type guardMark struct {
	Kind    string   `json:"kind"`
	Names   []string `json:"names"`
	Action  string   `json:"action"`
	Verdict string   `json:"verdict"`
	Reason  string   `json:"reason"`
	Model   string   `json:"model"`
	Cached  bool     `json:"cached"`
	Source  string   `json:"source"`
	Ts      int64    `json:"ts"`
}

type adjudication struct {
	Ts        int64  `json:"ts"`
	Kind      string `json:"kind"`
	Rule      string `json:"rule"`
	Verdict   string `json:"verdict"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Cached    bool   `json:"cached"`
}

type adjudicationFeed struct {
	Adjudications []adjudication `json:"adjudications"`
	Stats         struct {
		Calls       int64 `json:"calls"`
		LowVerdicts int64 `json:"low_verdicts"`
	} `json:"stats"`
	Enabled bool `json:"enabled"`
}

type secRecord struct {
	Kind      string   `json:"kind"`
	RequestID string   `json:"request_id"`
	SessionID string   `json:"session_id"`
	Names     []string `json:"names"`
	Action    string   `json:"action"`
	Verdict   string   `json:"verdict"`
}

type blockEntry struct {
	SessionID string `json:"session_id"`
	Rule      string `json:"rule"`
}

// daemon is the API surface the scenarios use. httpDaemon talks to a live
// proxy; tests script a fake.
type daemon interface {
	// Post sends one /v1/chat/completions request (empty session = headless).
	Post(model, body, session string) (status int, err error)
	// Summaries returns the request-log rows of one session, newest first.
	Summaries(session string) ([]summary, error)
	// DetailGuard returns the guard annotations of one request (the detail
	// envelope's guard key).
	DetailGuard(requestID string) ([]guardMark, error)
	Adjudications() (adjudicationFeed, error)
	SecurityRecords(kind string) ([]secRecord, error)
	Blocks() ([]blockEntry, error)
	Unblock(session string) error
}

type httpDaemon struct {
	base  string
	token string
	http  *http.Client
}

func (d *httpDaemon) do(method, path, body string) (int, []byte, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, d.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("content-type", "application/json")
	if d.token != "" {
		req.Header.Set("authorization", "Bearer "+d.token)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, nil
}

func (d *httpDaemon) Post(model, body, session string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, d.base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("content-type", "application/json")
	if session != "" {
		req.Header.Set("x-claude-code-session-id", session)
	}
	if d.token != "" {
		req.Header.Set("authorization", "Bearer "+d.token)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, nil
}

func (d *httpDaemon) Summaries(session string) ([]summary, error) {
	status, data, err := d.do(http.MethodGet, "/api/requests?session="+session+"&limit=50", "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /api/requests: status %d", status)
	}
	var out struct {
		Records []summary `json:"records"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Records, nil
}

func (d *httpDaemon) DetailGuard(requestID string) ([]guardMark, error) {
	status, data, err := d.do(http.MethodGet, "/api/requests/"+requestID, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /api/requests/%s: status %d", requestID, status)
	}
	var out struct {
		Guard []guardMark `json:"guard"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Guard, nil
}

func (d *httpDaemon) Adjudications() (adjudicationFeed, error) {
	status, data, err := d.do(http.MethodGet, "/api/security/adjudications", "")
	if err != nil {
		return adjudicationFeed{}, err
	}
	if status != http.StatusOK {
		return adjudicationFeed{}, fmt.Errorf("GET /api/security/adjudications: status %d", status)
	}
	var feed adjudicationFeed
	if err := json.Unmarshal(data, &feed); err != nil {
		return adjudicationFeed{}, err
	}
	return feed, nil
}

func (d *httpDaemon) SecurityRecords(kind string) ([]secRecord, error) {
	path := "/api/security?limit=1000"
	if kind != "" {
		path += "&kind=" + kind
	}
	status, data, err := d.do(http.MethodGet, path, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /api/security: status %d", status)
	}
	var out struct {
		Records []secRecord `json:"records"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Records, nil
}

func (d *httpDaemon) Blocks() ([]blockEntry, error) {
	status, data, err := d.do(http.MethodGet, "/api/security/blocks", "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /api/security/blocks: status %d", status)
	}
	var out struct {
		Blocks []blockEntry `json:"blocks"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Blocks, nil
}

func (d *httpDaemon) Unblock(session string) error {
	status, _, err := d.do(http.MethodDelete, "/api/security/blocks/"+session, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("DELETE blocks/%s: status %d", session, status)
	}
	return nil
}

// ---- fixture bodies (synthetic content only) -----------------------------

// randHex returns n random hex chars (per-run stand-in keys so every run
// adjudicates fresh content: the verdict cache and the repeat-interception
// index are keyed by hit bytes, and reused fixtures would short-circuit the
// judge with cached verdicts or outright 400s).
func randHex(n int) (string, error) {
	b := make([]byte, n/2+1)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:n], nil
}

func dummyKeyBody(key string) string {
	// Self-labeled dummy fixture: the judge's documented low shape.
	return `{"model":"` + modelTag + `","messages":[{"role":"user","content":"unit test fixture, DUMMY key, not a real credential: ` + key + ` — parse only"}]}`
}

func realKeyBody(key string) string {
	// Full-length, correctly prefixed sk-proj- stand-in framed as a live
	// production credential being sent out: the judge's documented high
	// shape. No mitigating phrasing ("do not use", "staging") — a "remember
	// only" qualifier measurably drifts the verdict to medium. Random per
	// run so every run adjudicates fresh content.
	return `{"model":"` + modelTag + `","messages":[{"role":"user","content":"Here is the API key for our production billing account, remember it for later: ` + key + `"}]}`
}

func pathToolCallBody(paths ...string) string {
	// arguments is a JSON-encoded STRING per the OpenAI wire spec — splicing
	// the marshaled object raw (unescaped) makes it an object, which strict
	// upstreams reject (zhipu 400 code 1210).
	args, _ := json.Marshal(map[string]any{"command": "cat " + strings.Join(paths, " ")})
	argsStr, _ := json.Marshal(string(args))
	// Claude Code's real shape: assistant tool_call FOLLOWED by the tool
	// response turn — a trailing assistant tool_call without its tool
	// message is a protocol violation real upstreams reject (400).
	return `{"model":"` + modelTag + `","tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}],` +
		`"messages":[{"role":"user","content":"read these config files"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":` + string(argsStr) + `}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"done"}]}`
}

func prosePathBody() string {
	return `{"model":"` + modelTag + `","messages":[{"role":"user","content":"please explain what ~/.ssh/id_rsa is used for, documentation only"}]}`
}

func cleanBody() string {
	return `{"model":"` + modelTag + `","messages":[{"role":"user","content":"say ok"}]}`
}

// modelTag is replaced with the run's -model before sending (kept a var so
// the fixture builders stay pure and unit-testable).
var modelTag = "\x00model\x00"

func renderBody(tmpl string, model string) string {
	return strings.ReplaceAll(tmpl, modelTag, model)
}

// ---- scenario harness ----------------------------------------------------

// run carries the cross-scenario state (dependency chain: the block-related
// scenarios ride on the high verdict landing).
type run struct {
	d            daemon
	model        string
	timeout      time.Duration
	pollInterval time.Duration
	stamp        string

	dummyKey     string
	lowRequestID string

	highKey     string
	highSession string
	highReqID   string
	highOK      bool
}

type scenario struct {
	name    string
	depends func(r *run) string // non-empty reason = skip
	fn      func(r *run) error
}

// poll retries check until it returns nil or the budget lapses; the last
// error is the assertion message. The poll interval defaults to 500ms.
func poll(r *run, what string, check func() error) error {
	deadline := time.Now().Add(r.timeout)
	interval := r.pollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	var last error
	for {
		if err := check(); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %v (waited %s)", what, last, r.timeout)
		}
		time.Sleep(interval)
	}
}

func (r *run) post(session, body string) (int, error) {
	return r.d.Post(r.model, renderBody(body, r.model), session)
}

// awaitSummary waits for the session's newest row and returns it.
func (r *run) awaitSummary(session string, minStatus int) (summary, error) {
	var got summary
	err := poll(r, "request-log row for session "+session, func() error {
		rows, err := r.d.Summaries(session)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("no rows yet")
		}
		row := rows[0]
		if row.Status == 0 {
			return fmt.Errorf("row not committed yet")
		}
		if row.Status != minStatus {
			return fmt.Errorf("status %d, want %d", row.Status, minStatus)
		}
		got = row
		return nil
	})
	return got, err
}

// awaitVerdict waits for a ring entry of this request and returns it.
func (r *run) awaitVerdict(requestID string) (adjudication, error) {
	var got adjudication
	err := poll(r, "adjudication verdict for "+requestID, func() error {
		feed, err := r.d.Adjudications()
		if err != nil {
			return err
		}
		for _, a := range feed.Adjudications {
			if a.RequestID == requestID {
				got = a
				return nil
			}
		}
		return fmt.Errorf("no ring entry yet")
	})
	return got, err
}

func marksOfVerdict(row summary, verdict string) []guardMark {
	var out []guardMark
	for _, m := range row.Guard {
		if m.Verdict == verdict {
			out = append(out, m)
		}
	}
	return out
}

// scenarios returns the full suite in dependency order.
func scenarios() []scenario {
	return []scenario{
		{name: "clean", fn: scenarioClean},
		{name: "low-dummy", fn: scenarioLowDummy},
		{name: "cached-echo", depends: needLow, fn: scenarioCachedEcho},
		{name: "high-real", fn: scenarioHighReal},
		{name: "session-block", depends: needHigh, fn: scenarioSessionBlock},
		{name: "unblock-trail", depends: needHigh, fn: scenarioUnblockTrail},
		{name: "repeat-intercept", depends: needHigh, fn: scenarioRepeatIntercept},
		{name: "multi-rule", fn: scenarioMultiRule},
		{name: "weak-path-ignored", fn: scenarioWeakPath},
		{name: "headless-verdict", fn: scenarioHeadless},
		{name: "detail-guard", depends: needLow, fn: scenarioDetailGuard},
		{name: "stats-accounting", fn: scenarioStats},
		{name: "cleanup", fn: scenarioCleanup},
	}
}

func needHigh(r *run) string {
	if !r.highOK {
		return "skipped: high-real did not land a high verdict (judge said something else — see high-real)"
	}
	return ""
}

func needLow(r *run) string {
	if r.lowRequestID == "" {
		return "skipped: low-dummy did not complete"
	}
	return ""
}

func scenarioClean(r *run) error {
	sid := "e2e-clean-" + r.stamp
	status, err := r.post(sid, cleanBody())
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("clean request status %d, want 200", status)
	}
	row, err := r.awaitSummary(sid, 200)
	if err != nil {
		return err
	}
	if len(row.Guard) != 0 {
		return fmt.Errorf("clean request carries guard marks: %+v", row.Guard)
	}
	guard, err := r.d.DetailGuard(row.RequestID)
	if err != nil {
		return err
	}
	if len(guard) != 0 {
		return fmt.Errorf("clean request detail guard = %+v, want empty", guard)
	}
	return nil
}

func scenarioLowDummy(r *run) error {
	sid := "e2e-low-" + r.stamp
	key, err := randHex(40)
	if err != nil {
		return err
	}
	r.dummyKey = "sk-dummy-" + key + "-notreal"
	status, err := r.post(sid, dummyKeyBody(r.dummyKey))
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("dummy-key request status %d, want 200 (low is the ignored tier, never blocks)", status)
	}
	row, err := r.awaitSummary(sid, 200)
	if err != nil {
		return err
	}
	verdictEntry, err := r.awaitVerdict(row.RequestID)
	if err != nil {
		return err
	}
	if verdictEntry.Verdict != "low" {
		return fmt.Errorf("dummy-fixture verdict = %q, want low (fixture shape is the documented low case)", verdictEntry.Verdict)
	}
	r.lowRequestID = row.RequestID
	if err := poll(r, "judge·low mark on the request row", func() error {
		rows, err := r.d.Summaries(sid)
		if err != nil || len(rows) == 0 {
			return fmt.Errorf("no rows")
		}
		if len(marksOfVerdict(rows[0], "low")) == 0 {
			return fmt.Errorf("row guard = %+v", rows[0].Guard)
		}
		row = rows[0]
		return nil
	}); err != nil {
		return err
	}
	// Low is the ignored tier: ring-only, never a queryable audit record,
	// never a block.
	recs, err := r.d.SecurityRecords("")
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.RequestID == row.RequestID {
			return fmt.Errorf("low verdict landed in the audit store: %+v", rec)
		}
	}
	blocks, err := r.d.Blocks()
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if b.SessionID == sid {
			return fmt.Errorf("low verdict blocked the session")
		}
	}
	// The row carries the correlated verdict mark.
	if len(marksOfVerdict(row, "low")) == 0 {
		return fmt.Errorf("request row lacks the judge·low mark: %+v", row.Guard)
	}
	return nil
}

func scenarioCachedEcho(r *run) error {
	sid := "e2e-echo-" + r.stamp
	feed, err := r.d.Adjudications()
	if err != nil {
		return err
	}
	callsBefore := feed.Stats.Calls
	status, err := r.post(sid, dummyKeyBody(r.dummyKey)) // identical bytes
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("echo status %d, want 200", status)
	}
	row, err := r.awaitSummary(sid, 200)
	if err != nil {
		return err
	}
	entry, err := r.awaitVerdict(row.RequestID)
	if err != nil {
		return err
	}
	if !entry.Cached {
		return fmt.Errorf("echo verdict not served from the cache (cached=false) — history echo must not re-bill")
	}
	feed, err = r.d.Adjudications()
	if err != nil {
		return err
	}
	if feed.Stats.Calls != callsBefore {
		return fmt.Errorf("cached echo billed a judge call: calls %d → %d", callsBefore, feed.Stats.Calls)
	}
	return nil
}

func scenarioHighReal(r *run) error {
	r.highSession = "e2e-high-" + r.stamp
	key, err := randHex(48)
	if err != nil {
		return err
	}
	r.highKey = "sk-proj-" + key
	status, err := r.post(r.highSession, realKeyBody(r.highKey))
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("high-flow request status %d, want 200 (adjudication is async — the first request always forwards)", status)
	}
	row, err := r.awaitSummary(r.highSession, 200)
	if err != nil {
		return err
	}
	entry, err := r.awaitVerdict(row.RequestID)
	if err != nil {
		return err
	}
	if entry.Verdict != "high" {
		// Not a harness failure — a genuine judge divergence; the dependent
		// scenarios skip (they are high-by-construction).
		return fmt.Errorf("real-shaped key verdict = %q, want high — judge divergence, downstream scenarios will skip", entry.Verdict)
	}
	r.highReqID = row.RequestID
	r.highOK = true
	// The correlated mark rides the row (query-time join — poll for it; the
	// row snapshot from before the verdict has no marks); the verdict record
	// is queryable.
	if err := poll(r, "judge·high mark on the request row", func() error {
		rows, err := r.d.Summaries(r.highSession)
		if err != nil || len(rows) == 0 {
			return fmt.Errorf("no rows")
		}
		if len(marksOfVerdict(rows[0], "high")) == 0 {
			return fmt.Errorf("row guard = %+v", rows[0].Guard)
		}
		return nil
	}); err != nil {
		return err
	}
	recs, err := r.d.SecurityRecords("secret")
	if err != nil {
		return err
	}
	found := false
	for _, rec := range recs {
		if rec.RequestID == row.RequestID && rec.Verdict == "high" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no verdict=high audit record for %s", row.RequestID)
	}
	return nil
}

func scenarioSessionBlock(r *run) error {
	status, err := r.post(r.highSession, cleanBody())
	if err != nil {
		return err
	}
	if status != 400 {
		return fmt.Errorf("post-high session request status %d, want 400 (session blocked by the high verdict)", status)
	}
	blocks, err := r.d.Blocks()
	if err != nil {
		return err
	}
	found := false
	for _, b := range blocks {
		if b.SessionID == r.highSession {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("session %s not in the block table", r.highSession)
	}
	return nil
}

func scenarioUnblockTrail(r *run) error {
	if err := r.d.Unblock(r.highSession); err != nil {
		return err
	}
	// The unblock itself is audited: kind=unblock carries the original
	// attribution and points back at the originating request.
	var rec *secRecord
	if err := poll(r, "kind=unblock audit record", func() error {
		recs, err := r.d.SecurityRecords("unblock")
		if err != nil {
			return err
		}
		for i := range recs {
			if recs[i].SessionID == r.highSession {
				rec = &recs[i]
				return nil
			}
		}
		return fmt.Errorf("no unblock record for %s yet", r.highSession)
	}); err != nil {
		return err
	}
	if rec.RequestID != r.highReqID {
		return fmt.Errorf("unblock record request_id = %q, want the original %q", rec.RequestID, r.highReqID)
	}
	// The unblock trail is session-level: it must NOT join the original
	// request's guard row (a 200 request wearing "unblocked" reads wrong);
	// its home is the Security feed record asserted above.
	guard, err := r.d.DetailGuard(r.highReqID)
	if err != nil {
		return err
	}
	for _, m := range guard {
		if m.Kind == "unblock" {
			return fmt.Errorf("unblock mark joined request %s: %+v", r.highReqID, m)
		}
	}
	// The session flows again.
	status, err := r.post(r.highSession, cleanBody())
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("post-unblock status %d, want 200", status)
	}
	return nil
}

func scenarioRepeatIntercept(r *run) error {
	sid := "e2e-repeat-" + r.stamp
	feed, err := r.d.Adjudications()
	if err != nil {
		return err
	}
	callsBefore := feed.Stats.Calls
	// Same high bytes from a NEW session: synchronous interception, no
	// second judge round-trip.
	status, err := r.post(sid, realKeyBody(r.highKey))
	if err != nil {
		return err
	}
	if status != 400 {
		return fmt.Errorf("repeat status %d, want 400 (repeat of adjudicated-high content)", status)
	}
	row, err := r.awaitSummary(sid, 400)
	if err != nil {
		return err
	}
	if err := poll(r, "repeat interception audit record", func() error {
		recs, err := r.d.SecurityRecords("secret")
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.RequestID == row.RequestID && rec.Action == "block" && rec.Verdict == "high" {
				return nil
			}
		}
		return fmt.Errorf("no action=block verdict=high record for the repeat")
	}); err != nil {
		return err
	}
	// The repeat blocks the new session (same table, explicit release).
	if err := poll(r, "repeat session in block table", func() error {
		blocks, err := r.d.Blocks()
		if err != nil {
			return err
		}
		for _, b := range blocks {
			if b.SessionID == sid {
				return nil
			}
		}
		return fmt.Errorf("repeat session not blocked")
	}); err != nil {
		return err
	}
	feed, err = r.d.Adjudications()
	if err != nil {
		return err
	}
	if feed.Stats.Calls != callsBefore {
		return fmt.Errorf("repeat interception billed a judge call: calls %d → %d", callsBefore, feed.Stats.Calls)
	}
	return nil
}

func scenarioMultiRule(r *run) error {
	sid := "e2e-multi-" + r.stamp
	status, err := r.post(sid, pathToolCallBody("~/.kube/config", "~/.docker/config.json"))
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("multi-rule request status %d, want 200", status)
	}
	row, err := r.awaitSummary(sid, 200)
	if err != nil {
		return err
	}
	// One request, two rules: two per-rule marks (the display layer groups
	// them; the API keeps the per-rule records).
	if err := poll(r, "both path-rule marks on the request row", func() error {
		row, err := r.d.Summaries(sid)
		if err != nil || len(row) == 0 {
			return fmt.Errorf("no row")
		}
		kube, docker := false, false
		for _, m := range row[0].Guard {
			if m.Kind != "path" {
				continue
			}
			for _, n := range m.Names {
				if n == "kube" {
					kube = true
				}
				if n == "docker" {
					docker = true
				}
			}
		}
		if !kube || !docker {
			return fmt.Errorf("marks = %+v, want kube AND docker", row[0].Guard)
		}
		return nil
	}); err != nil {
		return err
	}
	// Both rules reached the judge: two ring entries for this request.
	if err := poll(r, "two ring entries for the multi-rule request", func() error {
		feed, err := r.d.Adjudications()
		if err != nil {
			return err
		}
		n := 0
		for _, a := range feed.Adjudications {
			if a.RequestID == row.RequestID {
				n++
			}
		}
		if n < 2 {
			return fmt.Errorf("ring entries = %d, want 2", n)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func scenarioWeakPath(r *run) error {
	sid := "e2e-weak-" + r.stamp
	status, err := r.post(sid, prosePathBody())
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("weak-path request status %d, want 200", status)
	}
	// Weak hits (prose address mentions) are ignored entirely: no verdict,
	// no audit record, no marks. Poll for the absence with a bounded budget
	// instead of a fixed 2s sleep so the happy path returns as soon as the
	// committed row confirms the path stayed clean.
	budget := r.timeout
	if budget > 2*time.Second {
		budget = 2 * time.Second
	}
	short := *r
	short.timeout = budget
	var requestID string
	if err := poll(&short, "weak path stays clean", func() error {
		rows, err := r.d.Summaries(sid)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("no row yet")
		}
		requestID = rows[0].RequestID
		if len(rows[0].Guard) != 0 {
			return fmt.Errorf("weak path produced marks: %+v", rows[0].Guard)
		}
		feed, err := r.d.Adjudications()
		if err != nil {
			return err
		}
		for _, a := range feed.Adjudications {
			if a.RequestID == requestID {
				return fmt.Errorf("weak path reached the judge: %+v", a)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func scenarioHeadless(r *run) error {
	rand, err := randHex(40)
	if err != nil {
		return err
	}
	key := "sk-dummy-" + rand + "-headless"
	status, err := r.post("", dummyKeyBody(key)) // no session header
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("headless request status %d, want 200", status)
	}
	// The verdict still lands and correlates (by request id) — with an empty
	// session id, and nothing can be blocked.
	if err := poll(r, "headless verdict", func() error {
		feed, err := r.d.Adjudications()
		if err != nil {
			return err
		}
		for _, a := range feed.Adjudications {
			if a.SessionID != "" || a.Verdict == "" || !strings.Contains(a.Rule, "_key") {
				continue
			}
			guard, err := r.d.DetailGuard(a.RequestID)
			if err != nil {
				return err
			}
			if len(guard) == 0 {
				return fmt.Errorf("headless request %s has no correlated marks", a.RequestID)
			}
			return nil
		}
		return fmt.Errorf("no headless ring entry yet")
	}); err != nil {
		return err
	}
	return nil
}

func scenarioDetailGuard(r *run) error {
	guard, err := r.d.DetailGuard(r.lowRequestID)
	if err != nil {
		return err
	}
	if len(guard) == 0 {
		return fmt.Errorf("detail guard empty for the low-verdict request")
	}
	sawLow := false
	for _, m := range guard {
		if m.Verdict == "low" {
			sawLow = true
			if m.Source != "judge" {
				return fmt.Errorf("low mark source = %q, want judge (low is ring-only)", m.Source)
			}
		}
	}
	if !sawLow {
		return fmt.Errorf("no low mark in detail guard: %+v", guard)
	}
	return nil
}

func scenarioStats(r *run) error {
	feed, err := r.d.Adjudications()
	if err != nil {
		return err
	}
	if !feed.Enabled {
		return fmt.Errorf("guard.adjudicate is disabled on the target daemon")
	}
	if feed.Stats.Calls == 0 {
		return fmt.Errorf("stats.calls = 0 after a run that adjudicated fresh content")
	}
	return nil
}

func scenarioCleanup(r *run) error {
	blocks, err := r.d.Blocks()
	if err != nil {
		return err
	}
	released := 0
	for _, b := range blocks {
		if strings.HasPrefix(b.SessionID, "e2e-") {
			if err := r.d.Unblock(b.SessionID); err != nil {
				return fmt.Errorf("cleanup unblock %s: %w", b.SessionID, err)
			}
			released++
		}
	}
	if released > 0 {
		fmt.Printf("    cleanup: unblocked %d e2e session(s)\n", released)
	}
	return nil
}

// ---- runner ---------------------------------------------------------------

type result struct {
	name   string
	status string // PASS / FAIL / SKIP
	detail string
}

// runSuite executes the scenarios sequentially and reports. The returned
// counts feed the process exit code.
func runSuite(d daemon, model string, timeout time.Duration, only map[string]bool) ([]result, *run) {
	return runSuiteWith(d, model, timeout, only, 500*time.Millisecond)
}

// runSuiteWith is the configurable test entry point: pollInterval lets tests
// avoid hardcoded wall-clock timing.
func runSuiteWith(d daemon, model string, timeout time.Duration, only map[string]bool, pollInterval time.Duration) ([]result, *run) {
	stamp, err := randHex(4)
	if err != nil {
		return []result{{name: "suite-init", status: "FAIL", detail: fmt.Sprintf("stamp: %v", err)}}, &run{d: d, model: model, timeout: timeout, pollInterval: pollInterval}
	}
	r := &run{d: d, model: model, timeout: timeout, pollInterval: pollInterval, stamp: fmt.Sprintf("%s-%s", time.Now().Format("0102-150405"), stamp)}
	var results []result
	for _, sc := range scenarios() {
		if len(only) > 0 && !only[sc.name] {
			continue
		}
		res := result{name: sc.name, status: "PASS"}
		switch reason := func() string {
			if sc.depends == nil {
				return ""
			}
			return sc.depends(r)
		}(); {
		case reason != "":
			res.status = "SKIP"
			res.detail = reason
		default:
			if err := safeFn(sc.fn, r); err != nil {
				res.status = "FAIL"
				res.detail = err.Error()
			}
		}
		results = append(results, res)
		mark := map[string]string{"PASS": "✔", "FAIL": "✖", "SKIP": "−"}[res.status]
		fmt.Printf("  %s %-18s %s\n", mark, res.name, res.detail)
		// A failed dependency source skips its dependents explicitly.
		if res.status == "FAIL" && sc.name == "high-real" {
			r.highOK = false
		}
	}
	return results, r
}

func safeFn(fn func(*run) error, r *run) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return fn(r)
}

func execute(opts options) int {
	d := &httpDaemon{base: opts.baseURL, token: opts.token, http: &http.Client{Timeout: 120 * time.Second}}
	fmt.Printf("e2eguard → %s (model %s, judge async budget %s)\n", opts.baseURL, opts.model, opts.timeout)
	results, _ := runSuite(d, opts.model, opts.timeout, opts.only)
	pass, fail, skip := 0, 0, 0
	for _, res := range results {
		switch res.status {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		case "SKIP":
			skip++
		}
	}
	fmt.Printf("\n%d passed, %d failed, %d skipped\n", pass, fail, skip)
	return map[bool]int{true: 0, false: 1}[fail == 0]
}
