package app

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"model-proxy/internal/guard"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/seclog"
)

// Split-exfiltration (session scan) integration: a pool key fragmented across
// multiple requests of one x-claude-code-session-id session must fire
// known_secret_fragmented; single-request / cross-session / headerless cases
// must not. All fixtures are synthetic — never real credentials (AGENTS.md
// credential red line), and failure output must not echo fixture bytes.

// fragPoolKey is a synthetic 40-char pool key, splittable into fragments that
// each clear guard's minKnownFrag (8).
var fragPoolKey = "poolkey-" + strings.Repeat("zK7v", 8)

func fragBody(fragment string) string {
	return `{"model":"glm","messages":[{"role":"user","content":"note ` + fragment + `"}]}`
}

// postSession posts body with an optional x-claude-code-session-id header.
func postSession(t *testing.T, url, body, session string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", url, stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	if session != "" {
		req.Header.Set("x-claude-code-session-id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// mustGuardScanner builds a known-secret scanner for store-level tests; each
// call returns a distinct pointer (a fresh "generation").
func mustGuardScanner(t *testing.T) *guard.Scanner {
	t.Helper()
	sc, err := guard.NewScannerWithOptions(nil, []string{fragPoolKey}, nil, guard.Options{Decode: true})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// fragmentedEvents returns the guard event Details naming
// known_secret_fragmented.
func fragmentedEvents(p *Proxy) []string {
	var out []string
	for _, d := range guardEventDetails(p) {
		if strings.Contains(d, "known_secret_fragmented") {
			out = append(out, d)
		}
	}
	return out
}

func fragmentedCount(p *Proxy) uint64 {
	return p.metrics.Snapshot()[counters.PMKey{Provider: "guard", Model: "known_secret_fragmented"}].Requests
}

func sessionGuardCfg(action string) GuardConfig {
	return GuardConfig{Secrets: action, KnownSecrets: true, Decode: true, SessionScan: true}
}

// (a) Two-request split: the second request completes the key → one
// fragmented event + counter, both bodies forwarded (log action), no secret
// material in the event.
func TestSessionScan_TwoFragmentSplitFires(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("first fragment must not fire, events = %v", got)
	}
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")

	details := fragmentedEvents(p)
	if len(details) != 1 {
		t.Fatalf("fragmented events = %v, want exactly 1", details)
	}
	if !strings.Contains(details[0], "action=log") {
		t.Errorf("fragmented event detail = %q, want action=log", details[0])
	}
	if strings.Contains(details[0], fragPoolKey[:16]) {
		t.Errorf("fragmented event leaked key material")
	}
	if n := fragmentedCount(p); n != 1 {
		t.Errorf("fragmented counter = %d, want 1", n)
	}
	if got := bodies(); len(got) != 2 {
		t.Errorf("log action must forward both requests (calls=%d)", len(got))
	}
}

// (b) Three-request split fires only at the third request.
func TestSessionScan_ThreeFragmentSplitFiresAtThird(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:14]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[14:28]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("incomplete splits must not fire, events = %v", got)
	}
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[28:]), "s1")
	if got := fragmentedEvents(p); len(got) != 1 {
		t.Fatalf("fragmented events = %v, want exactly 1 after the third request", got)
	}
}

// (c) The full key in one request reports known_secret only — and a later
// clean request in the same session must NOT re-fire as fragmented just
// because the window still holds the key.
func TestSessionScan_FullKeySingleRequestNotFragmented(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("complete key in one body must not fire fragmented, events = %v", got)
	}
	snap := p.metrics.Snapshot()
	if n := snap[counters.PMKey{Provider: "guard", Model: "known_secret"}].Requests; n != 1 {
		t.Errorf("known_secret counter = %d, want 1", n)
	}
	postSession(t, proxyURL+"/v1/chat/completions", fragBody("clean follow-up"), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("window replay of an already-reported key must not fire fragmented, events = %v", got)
	}
}

// (d) Fragments in DIFFERENT sessions never reassemble; requests without the
// session header are not aggregated at all.
func TestSessionScan_SessionIsolation(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "session-a")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "session-b")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("cross-session/headerless fragments must not fire, events = %v", got)
	}
	// Sanity: the same split in ONE session does fire (fixture is valid).
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "session-c")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "session-c")
	if got := fragmentedEvents(p); len(got) != 1 {
		t.Fatalf("same-session split must fire, events = %v", got)
	}
}

// (e) Once >32KiB of later body traffic has pushed the first fragment out of
// the window, the split is NOT detected (documented bounded-window limit).
func TestSessionScan_WindowTruncationLosesFragments(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	junk := `{"model":"glm","messages":[{"role":"user","content":"` + strings.Repeat("y", 40<<10) + `"}]}`
	postSession(t, proxyURL+"/v1/chat/completions", junk, "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("fragments separated by >32KiB must not reassemble, events = %v", got)
	}
}

// (f) session_scan: false disables the whole pass (no events, no windows).
func TestSessionScan_ConfigFalseDisables(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t,
		GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true, SessionScan: false}, fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if got := fragmentedEvents(p); len(got) != 0 {
		t.Fatalf("session_scan=false must not fire, events = %v", got)
	}
	if n := p.sessionScan.Len(); n != 0 {
		t.Errorf("session_scan=false must not retain windows, sessions = %d", n)
	}
}

// (g) redact cannot rewrite a secret spanning requests: a fragmented hit
// degrades to log semantics (event says action=log; the fragment reaches the
// upstream unredacted).
func TestSessionScan_RedactDegradesToLog(t *testing.T) {
	p, proxyURL, bodies := newGuardPoolProxy(t, sessionGuardCfg("redact"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")

	details := fragmentedEvents(p)
	if len(details) != 1 || !strings.Contains(details[0], "action=log") {
		t.Fatalf("redact must degrade to log for fragmented hits, events = %v", details)
	}
	got := bodies()
	if len(got) != 2 || !strings.Contains(got[1], fragPoolKey[20:]) {
		t.Errorf("fragmented request under redact must forward unchanged (cannot rewrite across requests)")
	}
}

// (h) block rejects the completing request with 400 naming the signal, never
// the key; the first fragment passed through (unavoidable — it was clean).
func TestSessionScan_BlockRejectsCompletingRequest(t *testing.T) {
	_, proxyURL, bodies := newGuardPoolProxy(t, sessionGuardCfg("block"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	code, respBody := postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	if code != http.StatusBadRequest {
		t.Fatalf("block: status=%d body=%s, want 400", code, respBody)
	}
	if !strings.Contains(respBody, "known_secret_fragmented") {
		t.Errorf("block response = %q, want the signal name", respBody)
	}
	if strings.Contains(respBody, fragPoolKey[:16]) || strings.Contains(respBody, fragPoolKey[20:]) {
		t.Errorf("block response leaked key material")
	}
	if got := bodies(); len(got) != 1 {
		t.Errorf("blocked request reached the upstream %d times, want 0 (only the first fragment)", len(got))
	}
}

// (i) Fragmented hits persist a seclog record (kind=secret,
// names=[known_secret_fragmented]) with the effective action.
func TestSessionScan_AuditRecord(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)
	dir := t.TempDir()
	logger, err := seclog.New(dir, seclog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go logger.Run()
	p.secLog = logger

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[20:]), "s1")
	logger.Shutdown()

	result, err := seclog.Query(dir, seclog.Filter{Kind: seclog.KindSecret})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("secret records = %d, want 1", len(result.Records))
	}
	rec := result.Records[0]
	if len(rec.Names) != 1 || rec.Names[0] != "known_secret_fragmented" || rec.Action != "log" {
		t.Errorf("record = %+v, want names=[known_secret_fragmented] action=log", rec)
	}
	if rec.RequestID == "" || rec.Exposed != "glm" {
		t.Errorf("record missing request attribution: %+v", rec)
	}
}

// (j) Concurrent requests on one session: the completing fragment must fire
// at least once and the store must stay race-clean (run with -race).
func TestSessionScan_ConcurrentSameSession(t *testing.T) {
	p, proxyURL, _ := newGuardPoolProxy(t, sessionGuardCfg("log"), fragPoolKey)

	postSession(t, proxyURL+"/v1/chat/completions", fragBody(fragPoolKey[:20]), "s1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fragBody(fmt.Sprintf("padding-%d", i))
			if i == 0 {
				body = fragBody(fragPoolKey[20:])
			}
			req, err := http.NewRequest("POST", proxyURL+"/v1/chat/completions", stringReader(body))
			if err != nil {
				t.Errorf("new request: %v", err)
				return
			}
			req.Header.Set("x-claude-code-session-id", "s1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	if n := fragmentedCount(p); n < 1 {
		t.Errorf("fragmented counter = %d, want ≥ 1 (the completing request fired)", n)
	}
}

// Store-level bounds: tail truncation, LRU eviction, generation reset.
func TestSessionScanStore_Bounds(t *testing.T) {
	s := newSessionScanStore()
	sc := mustGuardScanner(t)

	// Tail truncation keeps only the last sessionScanWindowBytes.
	big := strings.Repeat("a", sessionScanWindowBytes+100)
	s.Add("s", []byte("HEAD-"+big), sc, nil)
	tail, _ := s.Snapshot("s", sc)
	if len(tail) != sessionScanWindowBytes {
		t.Errorf("window len = %d, want capped at %d", len(tail), sessionScanWindowBytes)
	}
	if strings.HasPrefix(string(tail), "HEAD-") {
		t.Errorf("window must keep the TAIL, not the head")
	}

	// LRU: more than sessionScanMaxSessions sessions evicts the oldest
	// (including "s", idle since before the flood).
	for i := 0; i < sessionScanMaxSessions+20; i++ {
		s.Add(fmt.Sprintf("sess-%d", i), []byte("x"), sc, nil)
	}
	if n := s.Len(); n != sessionScanMaxSessions {
		t.Errorf("sessions = %d, want capped at %d", n, sessionScanMaxSessions)
	}
	if tail, _ := s.Snapshot("sess-0", sc); tail != nil {
		t.Errorf("oldest session must be evicted")
	}
	if tail, _ := s.Snapshot(fmt.Sprintf("sess-%d", sessionScanMaxSessions+19), sc); tail == nil {
		t.Errorf("newest session must survive eviction")
	}

	// A scanner-generation change invalidates fragment progress.
	s2 := newSessionScanStore()
	s2.Add("g", []byte("body"), sc, []int{12})
	if _, progress := s2.Snapshot("g", mustGuardScanner(t)); progress != nil {
		t.Errorf("progress must reset across scanner generations, got %v", progress)
	}
}
