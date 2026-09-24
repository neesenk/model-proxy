package provider

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// zcodeWireCapture is one real model request from the open-sourced ZCode CLI,
// captured on a local server by scripts/zcode-wire-diff/run.sh (see
// scripts/zcode-wire-diff/README.md). It is the ground truth our fingerprint
// simulation must keep matching: if ZCode ships a version that changes the
// wire shape, this fixture goes stale and the parity check below fails until
// someone re-captures and reviews the diff.
type zcodeWireCapture struct {
	Source     string            `json:"source"`
	CapturedAt string            `json:"capturedAt"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Headers    map[string]string `json:"headers"`
}

func loadZCodeWireCapture(t *testing.T) zcodeWireCapture {
	t.Helper()
	b, err := os.ReadFile("testdata/zcode-wire/real-zcode-cli.json")
	if err != nil {
		t.Fatalf("read zcode wire fixture: %v (regenerate with scripts/zcode-wire-diff/run.sh)", err)
	}
	var c zcodeWireCapture
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse zcode wire fixture: %v", err)
	}
	if len(c.Headers) == 0 {
		t.Fatal("zcode wire fixture has no headers")
	}
	return c
}

// zcodeNodeMajorRe matches the runtime segment of the AI-SDK User-Agent, which
// reports the node MAJOR the client engine runs on. The capture host ran node
// 26; the shipped product pins node 24 (.nvmrc 24.14.0) and model-proxy sends
// 24, so the comparison normalizes the segment away.
var zcodeNodeMajorRe = regexp.MustCompile(`runtime/node\.js/\d+`)

func TestZCode_WireMatchesRealClientCapture(t *testing.T) {
	fx := loadZCodeWireCapture(t)

	// Build the outbound request exactly the way the forward path does
	// (targetexec/executor.go): a fresh request to the same method+path, then
	// AuthHeaders and ExtraHeaders in the same order. No client session headers
	// here — the session/query-id derivation from client hints has its own test
	// (TestZCode_SessionID_DerivedFromClientSession); this one is about the
	// fingerprint the real ZCode CLI puts on the wire.
	req, err := http.NewRequest(fx.Method, "http://127.0.0.1:8787"+fx.Path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "sk-wire-test")}
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	p.ExtraHeaders(req, fx.Path)

	// 1. Deterministic fingerprint values: exact match on both sides. A fixture
	//    mismatch here means ZCode changed the value (re-capture); a proxy
	//    mismatch means our implementation drifted.
	staticWant := map[string]string{
		"anthropic-version":    "2023-06-01",
		"http-referer":         "https://zcode.z.ai",
		"x-title":              "Z Code@" + zcodeSourceTitle,
		"x-zcode-agent":        "glm",
		"x-zcode-app-version":  zcodeAppVersion,
		"x-release-channel":    "production",
		"x-zcode-session-type": "main",
	}
	for h, want := range staticWant {
		if got := fx.Headers[h]; got != want {
			t.Errorf("fixture %s = %q, want %q — stale fixture, re-run scripts/zcode-wire-diff/run.sh", h, got, want)
		}
		if got := req.Header.Get(h); got != want {
			t.Errorf("our %s = %q, want %q", h, got, want)
		}
	}

	// 2. User-Agent: identical modulo the node major (see zcodeNodeMajorRe).
	//    Live-capture fact: there is NO "ai-sdk/anthropic/<ver>" segment — ZCode
	//    re-applies its own UA at the per-request header layer, overwriting the
	//    segment @ai-sdk/anthropic's getHeaders appends, and provider-utils then
	//    appends its suffix on top.
	normUA := func(ua string) string { return zcodeNodeMajorRe.ReplaceAllString(ua, "runtime/node.js/<major>") }
	if got, want := normUA(req.Header.Get("User-Agent")), normUA(fx.Headers["user-agent"]); got != want {
		t.Errorf("User-Agent = %q (normalized %q), want %q (normalized %q)",
			req.Header.Get("User-Agent"), got, fx.Headers["user-agent"], want)
	}

	// 3. Attribution ids: v4-UUID shaped on both sides (values are per-run).
	for _, h := range []string{"x-request-id", "x-session-id", "x-query-id", "x-zcode-trace-id"} {
		if got := fx.Headers[h]; !zcodeUUIDv4Re.MatchString(got) {
			t.Errorf("fixture %s = %q, want v4 UUID", h, got)
		}
		if got := req.Header.Get(h); !zcodeUUIDv4Re.MatchString(got) {
			t.Errorf("our %s = %q, want v4 UUID", h, got)
		}
	}

	// 4. Auth dual write with the same value in both headers (capture used a
	//    dummy key; the fixture redacts it to <API_KEY>).
	if got, want := req.Header.Get("Authorization"), "Bearer sk-wire-test"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := req.Header.Get("X-Api-Key"); got != "sk-wire-test" {
		t.Errorf("x-api-key = %q, want %q", got, "sk-wire-test")
	}
	if fx.Headers["authorization"] != "Bearer <API_KEY>" || fx.Headers["x-api-key"] != "<API_KEY>" {
		t.Errorf("fixture auth pair = %q / %q, want the redacted <API_KEY> pair (fixture tampered?)",
			fx.Headers["authorization"], fx.Headers["x-api-key"])
	}

	// 5. Machine/env-derived headers: the fixture value must be a non-empty
	//    printable string (its exact value is machine-bound), and OUR value must
	//    equal the algorithmic expectation for the machine running the test.
	for _, h := range []string{"x-platform", "x-os-category", "x-os-version", "x-client-language", "x-client-timezone"} {
		if v := fx.Headers[h]; v == "" || v != printableASCII(v) {
			t.Errorf("fixture %s = %q, want non-empty printable value", h, v)
		}
	}
	if got, want := req.Header.Get("X-Platform"), nodePlatform(runtime.GOOS)+"-"+nodeArch(runtime.GOARCH); got != want {
		t.Errorf("X-Platform = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("X-Os-Category"), osCategory(runtime.GOOS); got != want {
		t.Errorf("X-Os-Category = %q, want %q", got, want)
	}
	if fx.Headers["x-platform"] != req.Header.Get("X-Platform") ||
		fx.Headers["x-os-category"] != req.Header.Get("X-Os-Category") {
		t.Logf("note: fixture captured on %s/%s; this machine is %s/%s — machine-derived header values differ as expected",
			fx.Headers["x-platform"], fx.Headers["x-os-category"],
			nodePlatform(runtime.GOOS)+"-"+nodeArch(runtime.GOARCH), osCategory(runtime.GOOS))
	}

	// 6. Header-set parity — the drift detector. Everything the real client put
	//    on the wire (minus transport/client-config headers listed below) must
	//    also come from us, and we must not invent extras.
	ignore := map[string]bool{
		"host": true, "connection": true, "accept": true, "accept-encoding": true,
		"accept-language": true, "sec-fetch-mode": true, "content-length": true,
		"content-type": true,
		// Added by the AI SDK from the capture model's declared capabilities
		// (supportsMidConversationSystem). Real clients pass their own
		// anthropic-beta and targetexec forwards it via the header whitelist;
		// model-proxy does not synthesize one.
		"anthropic-beta": true,
	}
	for h := range fx.Headers {
		if ignore[h] {
			continue
		}
		if req.Header.Get(h) == "" {
			t.Errorf("real ZCode sends %q but model-proxy does not — fingerprint drift; re-run scripts/zcode-wire-diff/run.sh and review", h)
		}
	}
	for h := range req.Header {
		if ignore[strings.ToLower(h)] {
			continue
		}
		if _, ok := fx.Headers[strings.ToLower(h)]; !ok {
			t.Errorf("model-proxy sends %q but the real ZCode capture does not — extra fingerprint header", h)
		}
	}
}
