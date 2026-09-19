package forward

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/seclog"
)

// fakeAdjudicator captures enqueued jobs and scripts the session-block
// answers (the app-side adapter is covered in internal/app).
type fakeAdjudicator struct {
	mu                 sync.Mutex
	jobs               []GuardAdjudication
	enqueue            func() bool // script: nil = accept
	blocked            map[string][2]string
	blocks             []string // session ids added through BlockSession
	blockReasons       []string
	contentBlockedHits []string // hits passed through BlockSessionContent
}

func newFakeAdjudicator() *fakeAdjudicator {
	return &fakeAdjudicator{blocked: map[string][2]string{}, enqueue: func() bool { return true }}
}

// contentBlockedHit scripts ContentBlocked: when non-empty, exactly these hit
// bytes are reported as previously-adjudicated-high.
var contentBlockedHit string

func (f *fakeAdjudicator) ContentBlocked(hit string) (kind, rule, reason, evidence, model string, blocked bool) {
	if hit != contentBlockedHit {
		return "", "", "", "", "", false
	}
	return "secret", "openai_api_key", "repeat of earlier high verdict", "", "judge-model", true
}

func (f *fakeAdjudicator) Enqueue(a GuardAdjudication) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enqueue() {
		return false
	}
	f.jobs = append(f.jobs, a)
	return true
}

func (f *fakeAdjudicator) SessionBlocked(sessionID string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.blocked[sessionID]
	return v[0], v[1], ok
}

func (f *fakeAdjudicator) BlockSession(sessionID, rule, requestID, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[sessionID] = [2]string{rule, requestID}
	f.blockReasons = append(f.blockReasons, reason)
	f.blocks = append(f.blocks, sessionID)
}

func (f *fakeAdjudicator) BlockSessionContent(sessionID, rule, requestID, reason, hit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[sessionID] = [2]string{rule, requestID}
	f.blockReasons = append(f.blockReasons, reason)
	f.blocks = append(f.blocks, sessionID)
	f.contentBlockedHits = append(f.contentBlockedHits, hit)
}

func (f *fakeAdjudicator) captured() []GuardAdjudication {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]GuardAdjudication(nil), f.jobs...)
}

// TestExactSecretNames pins the exact-channel selection that the pipeline
// intercepts synchronously (everything else defers to adjudication).
func TestExactSecretNames(t *testing.T) {
	known := exactSecretNames([]string{"known_secret", "openai_api_key", "known_secret_encoded", "jwt"})
	if len(known) != 2 || known[0] != "known_secret" || known[1] != "known_secret_encoded" {
		t.Errorf("known = %v", known)
	}
	if known := exactSecretNames([]string{"openai_api_key", "jwt"}); known != nil {
		t.Errorf("pattern-only names = %v, want nil", known)
	}
}

// TestBuildAdjudications_SecretSpans: one job per DISTINCT matched span,
// capped; the hit stays raw while a known secret inside the context window
// is masked.
func TestBuildAdjudications_SecretSpans(t *testing.T) {
	sc := guardScanner(t, nil) // carries guardTestSecret as a known secret
	dummy := "sk-capture-dummy-not-a-real-key"
	body := []byte(secretBody(dummy + " plus " + dummy + " and known " + guardTestSecret))
	meta := GuardAdjudication{RequestID: "r1", SessionID: "s1", Action: "log"}
	jobs, leftover := buildAdjudications(sc, body, []string{"openai_api_key"}, AdjudicationKindSecret, false, 64, meta)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 (identical spans dedup)", len(jobs))
	}
	if len(leftover) != 0 {
		t.Errorf("leftover = %v, want none (the name produced a job)", leftover)
	}
	j := jobs[0]
	if j.Rule != "openai_api_key" || j.Kind != AdjudicationKindSecret {
		t.Errorf("job rule/kind = %s/%s", j.Rule, j.Kind)
	}
	if j.Hit != dummy {
		t.Errorf("hit = %q, want the raw span", j.Hit)
	}
	window := j.Pre + j.Hit + j.Post
	if strings.Contains(window, guardTestSecret) {
		t.Errorf("context window leaked the known secret: %q", window)
	}
	if !strings.Contains(window, "…") && j.Pre+j.Post != "" {
		t.Errorf("expected a mask marker in the context window: %q", window)
	}
	if j.RequestID != "r1" || j.SessionID != "s1" || j.Action != "log" {
		t.Errorf("meta not carried: %+v", j)
	}
}

// TestBuildAdjudications_StrongPathsOnly: weak path occurrences never build
// a job; strong (tool-call side) ones do.
func TestBuildAdjudications_StrongPathsOnly(t *testing.T) {
	sc := guardScanner(t, nil)
	strong := `{"model":"m","messages":[{"role":"user","content":"read"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"cat ~/.ssh/id_rsa"}}]}]}`
	weak := secretBody("the docs mention ~/.ssh in prose")
	meta := GuardAdjudication{RequestID: "r1"}
	if jobs, leftover := buildAdjudications(sc, []byte(weak), []string{"ssh"}, AdjudicationKindPath, true, 64, meta); len(jobs) != 0 {
		t.Errorf("weak body built %d jobs, want 0", len(jobs))
	} else if len(leftover) != 0 {
		t.Errorf("weak body produced leftover %v, want none (weak hits are ignored, not fail-opened)", leftover)
	}
	jobs, leftover := buildAdjudications(sc, []byte(strong), []string{"ssh"}, AdjudicationKindPath, true, 64, meta)
	// "~/.ssh" and "id_rsa" are two distinct literals of the same category.
	if len(jobs) != 2 {
		t.Fatalf("strong body jobs = %d, want 2 (one per distinct literal)", len(jobs))
	}
	if len(leftover) != 0 {
		t.Errorf("strong body leftover = %v, want none", leftover)
	}
	for _, j := range jobs {
		if j.Rule != "ssh" || j.Kind != AdjudicationKindPath {
			t.Errorf("job = %+v, want ssh path job", j)
		}
		if !strings.Contains(j.Pre+j.Hit+j.Post, "/.ssh") && !strings.Contains(j.Pre+j.Hit+j.Post, "id_rsa") {
			t.Errorf("strong job context lost the path: %+v", j)
		}
	}
}

// TestServeGuardAdjudicationDefersPatternHits: with the channel on, a
// pattern-table secret hit produces NO immediate guard event/audit — it is
// enqueued (the known-secret family is intercepted upstream of this test —
// see TestServeGuardExactMatchIntercepts).
func TestServeGuardAdjudicationDefersPatternHits(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off",
		Adjudicate: guardAdjudicateOn()}))
	snap.Guard = guardScanner(t, nil)

	dummy := "sk-capture-dummy-not-a-real-key"
	w := h.serve(snap, "openai", "/v1/chat/completions", secretBody(dummy), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (deferred channel never blocks synchronously)", w.Code)
	}
	jobs := adj.captured()
	if len(jobs) != 1 || jobs[0].Rule != "openai_api_key" {
		t.Fatalf("jobs = %+v, want one deferred openai_api_key", jobs)
	}
	for _, e := range h.events.Snapshot() {
		if e.Type == "guard" && strings.Contains(e.Detail, "openai_api_key") {
			t.Error("pattern hit emitted an immediate event under adjudication")
		}
	}
}

// TestServeGuardAdjudicationFailOpen: enqueue refusal (queue full) falls
// back to the classic immediate emit.
func TestServeGuardAdjudicationFailOpen(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	adj.enqueue = func() bool { return false }
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off",
		Adjudicate: guardAdjudicateOn()}))
	snap.Guard = guardScanner(t, nil)

	w := h.serve(snap, "openai", "/v1/chat/completions", secretBody("sk-capture-dummy-not-a-real-key"), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	for _, e := range h.events.Snapshot() {
		if e.Type == "guard" && strings.Contains(e.Detail, "openai_api_key") {
			return // classic emit happened
		}
	}
	t.Fatal("fail-open did not emit the classic immediate record")
}

// TestServeGuardAdjudicationBlockedSession: a blocked session 400s before
// scanning, with the unblock hint.
func TestServeGuardAdjudicationBlockedSession(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	adj.blocked["s-77"] = [2]string{"jwt", "req-9"}
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off"}))
	snap.Guard = guardScanner(t, nil)

	w := h.serve(snap, "openai", "/v1/chat/completions", secretBody("clean"), map[string]string{"x-claude-code-session-id": "s-77"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "guard unblock s-77") || !strings.Contains(w.Body.String(), "rule=jwt") {
		t.Errorf("block message = %q, want rule + unblock hint", w.Body.String())
	}
	// The upstream must never see the request.
	if up.hits() != 0 {
		t.Errorf("upstream saw %d requests, want 0", up.hits())
	}
}

// TestServeGuardAdjudicationBodyCarriedSession: agents without session
// headers (Codex on the Responses protocol carries
// client_metadata.session_id in the body) still get their session identity
// into the adjudication channel — a later high verdict must land in the
// block table and therefore the unblock list. Header values keep priority
// over the body field.
func TestServeGuardAdjudicationBodyCarriedSession(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off",
		Adjudicate: guardAdjudicateOn()}))
	snap.Guard = guardScanner(t, nil)

	body := `{"model":"m","client_metadata":{"session_id":"s-body-1","thread_id":"t-1"},"messages":[{"role":"user","content":"sk-capture-dummy-not-a-real-key"}]}`
	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (deferred channel never blocks synchronously)", w.Code)
	}
	jobs := adj.captured()
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].SessionID != "s-body-1" {
		t.Errorf("job session id = %q, want body-carried s-body-1", jobs[0].SessionID)
	}

	// A header, when present, outranks the body field.
	w = h.serve(snap, "openai", "/v1/chat/completions", body, map[string]string{"x-claude-code-session-id": "s-hdr-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	jobs = adj.captured()
	if len(jobs) != 2 || jobs[1].SessionID != "s-hdr-1" {
		t.Errorf("job session id = %+v, want header-carried s-hdr-1", jobs[1:])
	}
}

// TestServeGuardAdjudicationBlockedSessionFromBody: the block-table lookup
// uses the same derivation — a session blocked under its body-carried id is
// enforced even though the client sends no session header.
func TestServeGuardAdjudicationBlockedSessionFromBody(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	adj.blocked["s-body-1"] = [2]string{"jwt", "req-9"}
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off"}))
	snap.Guard = guardScanner(t, nil)

	body := `{"model":"m","client_metadata":{"session_id":"s-body-1"},"messages":[{"role":"user","content":"clean"}]}`
	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "guard unblock s-body-1") {
		t.Errorf("block message = %q, want unblock hint for s-body-1", w.Body.String())
	}
	if up.hits() != 0 {
		t.Errorf("upstream saw %d requests, want 0", up.hits())
	}
}

// TestServeGuardAdjudicationPathsDeferred: strong path hits defer too (the
// user-confirmed scope), weak ones stay ignored.
func TestServeGuardAdjudicationPathsDeferred(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "off", Paths: "log",
		Adjudicate: guardAdjudicateOn()}))
	snap.Guard = guardScanner(t, nil)

	body := `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"cat ~/.ssh/id_rsa"}}]}]}`
	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	jobs := adj.captured()
	if len(jobs) < 1 {
		t.Fatalf("jobs = %+v, want deferred ssh path job(s)", jobs)
	}
	for _, j := range jobs {
		if j.Rule != "ssh" || j.Kind != AdjudicationKindPath {
			t.Fatalf("jobs = %+v, want only ssh path jobs", jobs)
		}
	}
	for _, e := range h.events.Snapshot() {
		if e.Type == "guard" && strings.Contains(e.Detail, "ssh") {
			t.Error("strong path hit emitted an immediate event under adjudication")
		}
	}
}

// guardAdjudicateOn builds the minimal enabled AdjudicateConfig for tests.
func guardAdjudicateOn() configdomain.AdjudicateConfig {
	return configdomain.AdjudicateConfig{Enabled: true, Model: "judge", BlockSession: true}
}

// distinctDummyKeys builds n distinct rule-table-shaped keys (no key is a
// prefix of another, so Locate reports n separate spans of one name).
func distinctDummyKeys(n int) []string {
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		keys = append(keys, fmt.Sprintf("sk-capture-dummy-not-a-real-key-%02d", i))
	}
	return keys
}

// TestBuildAdjudications_CapOverflowFailsOpen pins the documented contract:
// spans past maxAdjudicationsPerRequest must NOT vanish — their names come
// back as leftover so the caller gives them the classic immediate record.
func TestBuildAdjudications_CapOverflowFailsOpen(t *testing.T) {
	sc := guardScanner(t, nil)
	keys := distinctDummyKeys(maxAdjudicationsPerRequest + 2)
	body := []byte(secretBody(strings.Join(keys, " ")))
	jobs, leftover := buildAdjudications(sc, body, []string{"openai_api_key"}, AdjudicationKindSecret, false, 64, GuardAdjudication{})
	if len(jobs) != maxAdjudicationsPerRequest {
		t.Fatalf("jobs = %d, want %d (per-request cap)", len(jobs), maxAdjudicationsPerRequest)
	}
	if len(leftover) != 1 || leftover[0] != "openai_api_key" {
		t.Fatalf("leftover = %v, want [openai_api_key] (overflow fails open)", leftover)
	}
	// Same BYTES under two path categories (builtin ssh literal "id_rsa" and
	// a custom path with the same literal): the second occurrence set dedups
	// onto the first name's job, and the deduped name still fails open —
	// its content was never adjudicated under that rule.
	dupSc := guardScanner(t, []string{"id_rsa"})
	dupBody := []byte(`{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"bash","input":{"command":"cat id_rsa id_rsa"}}]}]}`)
	jobs, leftover = buildAdjudications(dupSc, dupBody, []string{"ssh", "custom_path"}, AdjudicationKindPath, true, 64, GuardAdjudication{})
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want exactly one (identical bytes dedup across names)", jobs)
	}
	if len(leftover) != 1 || leftover[0] == jobs[0].Rule || (leftover[0] != "ssh" && leftover[0] != "custom_path") {
		t.Fatalf("leftover = %v, want the other name (deduped name fails open)", leftover)
	}
}

// TestServeGuardAdjudicationCapOverflowFailsOpen: a body stuffed past the
// cap keeps the classic immediate emit for the overflowing name — nothing
// disappears from the operator surfaces.
func TestServeGuardAdjudicationCapOverflowFailsOpen(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off",
		Adjudicate: guardAdjudicateOn()}))
	snap.Guard = guardScanner(t, nil)

	body := secretBody(strings.Join(distinctDummyKeys(maxAdjudicationsPerRequest+2), " "))
	audit, _ := serveCollectingAudit(t, h, &snap, body, nil, http.StatusOK)
	if len(adj.captured()) != maxAdjudicationsPerRequest {
		t.Fatalf("deferred jobs = %d, want %d", len(adj.captured()), maxAdjudicationsPerRequest)
	}
	found := false
	for _, r := range audit {
		if r.Kind == seclog.KindSecret && containsName(r.Names, "openai_api_key") {
			if r.Verdict != "skipped" {
				t.Errorf("overflow record verdict = %q, want skipped", r.Verdict)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("overflow past the cap wrote no classic audit record (silent drop)")
	}
}

// TestServeGuardAdjudicationKnownOnlyIsIntercepted pins the exact-match
// labeling: known-secret hits never defer to adjudication — the request is
// intercepted with an action=block record whose verdict is EMPTY (a classic
// record, not a fail-open "skipped").
func TestServeGuardAdjudicationKnownOnlyIsIntercepted(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off",
		Adjudicate: guardAdjudicateOn()}))
	snap.Guard = guardScanner(t, nil)

	audit, code := serveCollectingAudit(t, h, &snap, secretBody("token "+guardTestSecret), nil, http.StatusBadRequest)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the exact-match interception 400", code)
	}
	if n := len(adj.captured()); n != 0 {
		t.Fatalf("known-secret-only body enqueued %d jobs, want 0", n)
	}
	found := false
	for _, r := range audit {
		if containsName(r.Names, "known_secret") {
			if r.Verdict != "" {
				t.Errorf("interception record verdict = %q, want empty", r.Verdict)
			}
			if r.Action != "block" {
				t.Errorf("interception record action = %q, want block", r.Action)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("known-secret hit wrote no audit record")
	}
}

// serveCollectingAudit runs one request through the harness with a real
// (temp-dir) seclog logger on the snapshot and returns the flushed records
// plus the response status (wantCode asserts it).
func serveCollectingAudit(t *testing.T, h *harness, snap *Snapshot, body string, headers map[string]string, wantCode int) ([]seclog.Record, int) {
	t.Helper()
	logger, err := seclog.New(t.TempDir(), seclog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	snap.SecLog = logger
	go logger.Run()
	w := h.serve(*snap, "openai", "/v1/chat/completions", body, headers)
	if w.Code != wantCode {
		t.Fatalf("status = %d, want %d", w.Code, wantCode)
	}
	logger.Shutdown()
	return readAuditRecords(t, logger.Directory()), w.Code
}

// readAuditRecords parses every JSONL record the logger flushed to dir.
func readAuditRecords(t *testing.T, dir string) []seclog.Record {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []seclog.Record
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var r seclog.Record
			if json.Unmarshal([]byte(line), &r) != nil {
				continue // torn tail line
			}
			out = append(out, r)
		}
	}
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// TestServeGuardRepeatBlockedKeepsSiblingFailOpen: one rule with two distinct
// spans in one request — span A repeats previously-adjudicated-high content
// (repeat interception), span B's Enqueue fails (queue full). The repeat
// record covers segment A only; segment B must still get its fail-open
// record. A name-granular suppression would delete B's record because the
// RULE name already appears in the repeat record.
func TestServeGuardRepeatBlockedKeepsSiblingFailOpen(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	adj.enqueue = func() bool { return false } // every enqueue refuses
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off",
		Adjudicate: guardAdjudicateOn()}))
	snap.Guard = guardScanner(t, nil)

	keyA := "sk-capture-dummy-not-a-real-key-aa"
	keyB := "sk-capture-dummy-not-a-real-key-bb"
	contentBlockedHit = keyA
	defer func() { contentBlockedHit = "" }()

	audit, _ := serveCollectingAudit(t, h, &snap, secretBody(keyA+" and "+keyB), nil, http.StatusBadRequest)
	var repeat, failOpen bool
	for _, r := range audit {
		if r.Kind != seclog.KindSecret || !containsName(r.Names, "openai_api_key") {
			continue
		}
		switch {
		case r.Verdict == "high" && r.Action == "block":
			repeat = true // segment A: repeat interception record
		case r.Verdict == "skipped":
			failOpen = true // segment B: enqueue refusal fail-open record
		}
	}
	if !repeat {
		t.Error("repeat interception record (verdict=high action=block) missing")
	}
	if !failOpen {
		t.Error("sibling segment's fail-open record (verdict=skipped) was suppressed by name-granular dedup")
	}
}

// TestServeGuardRepeatBlockedWithChannelOff: the repeat-interception index
// is enforced even with the adjudicate channel disabled — the session-block
// table from the same high verdict already enforces with the channel off
// (TestServeGuardAdjudicationBlockedSession), and the two persistence faces
// of one verdict must not diverge when the operator stops the LLM spend.
func TestServeGuardRepeatBlockedWithChannelOff(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	h.svc.Adjudicator = adj
	// NO Adjudicate: guardAdjudicateOn() — the channel is OFF.
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off"}))
	snap.Guard = guardScanner(t, nil)

	key := "sk-capture-dummy-not-a-real-key-zz"
	contentBlockedHit = key
	defer func() { contentBlockedHit = "" }()

	audit, _ := serveCollectingAudit(t, h, &snap, secretBody(key), nil, http.StatusBadRequest)
	var repeat bool
	for _, r := range audit {
		if r.Kind == seclog.KindSecret && r.Verdict == "high" && r.Action == "block" && containsName(r.Names, "openai_api_key") {
			repeat = true
		}
	}
	if !repeat {
		t.Error("channel-off repeat interception record (verdict=high action=block) missing")
	}
	// Nothing may be enqueued with the channel off.
	if jobs := adj.captured(); len(jobs) != 0 {
		t.Errorf("channel off still enqueued adjudication jobs: %+v", jobs)
	}
}

// TestServeGuardChannelOffClassicRecordUnchanged: with the channel off and
// NO repeat hit, the classic immediate record keeps its pre-channel shape —
// verdict empty (not "skipped"), one record, request flows upstream.
func TestServeGuardChannelOffClassicRecordUnchanged(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	adj := newFakeAdjudicator()
	h.svc.Adjudicator = adj
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off"}))
	snap.Guard = guardScanner(t, nil)

	audit, _ := serveCollectingAudit(t, h, &snap, secretBody("sk-capture-dummy-not-a-real-key-cc"), nil, http.StatusOK)
	var classic int
	for _, r := range audit {
		if r.Kind == seclog.KindSecret && containsName(r.Names, "openai_api_key") {
			classic++
			if r.Verdict != "" {
				t.Errorf("channel-off classic record verdict = %q, want empty (nothing was deferred)", r.Verdict)
			}
			if r.Action != "log" {
				t.Errorf("channel-off classic record action = %q, want log", r.Action)
			}
		}
	}
	if classic != 1 {
		t.Errorf("classic records = %d, want 1", classic)
	}
	if jobs := adj.captured(); len(jobs) != 0 {
		t.Errorf("channel off enqueued jobs: %+v", jobs)
	}
}
