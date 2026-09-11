package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"testing"
	"time"
)

func TestZCode_AuthHeaders_SendsBothBearerAndXApiKey(t *testing.T) {
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "sk-test-123")}
	req, _ := http.NewRequest("POST", "https://open.bigmodel.cn/api/anthropic/v1/messages", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-test-123")
	}
	if got := req.Header.Get("x-api-key"); got != "sk-test-123" {
		t.Errorf("x-api-key = %q, want %q (ZCode sends BOTH)", got, "sk-test-123")
	}
}

func TestZCode_ExtraHeaders_Fingerprint(t *testing.T) {
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k")}
	req, _ := http.NewRequest("POST", "https://x/v1/messages", nil)
	p.ExtraHeaders(req, "/v1/messages")

	cases := map[string]string{
		"User-Agent":          "ZCode/3.11.2 ai-sdk/provider-utils/4.0.27 runtime/node.js/24",
		"HTTP-Referer":        "https://zcode.z.ai",
		"X-Title":             "Z Code@electron",
		"X-ZCode-App-Version": "3.11.2",
		"X-ZCode-Agent":       "glm",
		"X-Release-Channel":   "production",
		"anthropic-version":   "2023-06-01",
	}
	for h, want := range cases {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	// Runtime-derived: exact for THIS platform.
	if got, want := req.Header.Get("X-Platform"), nodePlatform(runtime.GOOS)+"-"+nodeArch(runtime.GOARCH); got != want {
		t.Errorf("X-Platform = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("X-Os-Category"), osCategory(runtime.GOOS); got != want {
		t.Errorf("X-Os-Category = %q, want %q", got, want)
	}
	// Language/timezone must always be non-empty (worst case "unknown").
	if req.Header.Get("X-Client-Language") == "" {
		t.Error("X-Client-Language empty")
	}
	if req.Header.Get("X-Client-Timezone") == "" {
		t.Error("X-Client-Timezone empty")
	}
	// X-Os-Version mirrors Node os.release(): non-empty and printable on
	// darwin/linux, omitted on windows (best-effort, ZCode omits when absent).
	if got := req.Header.Get("X-Os-Version"); runtime.GOOS != "windows" {
		if got == "" {
			t.Errorf("X-Os-Version empty on %s (want Node os.release() equivalent)", runtime.GOOS)
		}
	} else if got != "" {
		t.Errorf("X-Os-Version = %q on windows, want omitted", got)
	}
	// Packet-captured 2026-09-11: desktop sends per-request X-Request-Id and
	// per-session X-Session-Id v4 UUIDs.
	for _, h := range []string{"X-Request-Id", "X-Session-Id"} {
		if !zcodeUUIDv4Re.MatchString(req.Header.Get(h)) {
			t.Errorf("%s = %q, want v4 UUID", h, req.Header.Get(h))
		}
	}
	// Request id must differ per call; session id must stay stable.
	req2, _ := http.NewRequest("POST", "https://x/v1/messages", nil)
	p.ExtraHeaders(req2, "/v1/messages")
	if req.Header.Get("X-Request-Id") == req2.Header.Get("X-Request-Id") {
		t.Error("X-Request-Id identical across requests, want fresh per request")
	}
	if req.Header.Get("X-Session-Id") != req2.Header.Get("X-Session-Id") {
		t.Error("X-Session-Id changed across requests, want stable per process")
	}
}

var zcodeUUIDv4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestZCode_OsVersion_MatchesNodeOsRelease(t *testing.T) {
	v := osVersion()
	if v != printableASCII(v) {
		t.Errorf("osVersion() = %q not printable-normalized", v)
	}
	switch runtime.GOOS {
	case "darwin":
		// Node os.release() == kern.osrelease (kernel version), e.g. "25.6.0".
		if v == "" {
			t.Fatal("osVersion() empty on darwin")
		}
		if v[0] < '0' || v[0] > '9' {
			t.Errorf("osVersion() = %q, want kernel release starting with a digit", v)
		}
	case "linux":
		if v == "" {
			t.Fatal("osVersion() empty on linux")
		}
	}
}

func TestZCode_NodeNameMappings(t *testing.T) {
	plat := map[string]string{"darwin": "darwin", "windows": "win32", "linux": "linux"}
	for goos, want := range plat {
		if got := nodePlatform(goos); got != want {
			t.Errorf("nodePlatform(%q) = %q, want %q", goos, got, want)
		}
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64", "386": "ia32"}
	for goarch, want := range arch {
		if got := nodeArch(goarch); got != want {
			t.Errorf("nodeArch(%q) = %q, want %q", goarch, got, want)
		}
	}
	cat := map[string]string{"darwin": "macos", "windows": "windows", "linux": "linux"}
	for goos, want := range cat {
		if got := osCategory(goos); got != want {
			t.Errorf("osCategory(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestZCode_PrintableASCIIGuard(t *testing.T) {
	for in, want := range map[string]string{
		"zh-CN":            "zh-CN",
		"  Asia/Shanghai ": "Asia/Shanghai",
		"中文":               "", // non-ASCII → rejected
		"":                 "",
	} {
		if got := printableASCII(in); got != want {
			t.Errorf("printableASCII(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestZCode_ProbeRequest_AnthropicShape(t *testing.T) {
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k")}
	pr := p.ProbeRequest("glm-4.6")
	if pr.Method != http.MethodPost || pr.Path != "/v1/messages" {
		t.Errorf("ProbeRequest = {Method:%s Path:%s}, want POST /v1/messages", pr.Method, pr.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(pr.Body, &body); err != nil {
		t.Fatalf("probe body not json: %v", err)
	}
	if body["model"] != "glm-4.6" {
		t.Errorf("probe model = %v, want glm-4.6", body["model"])
	}
}

func TestZCode_Quota_ParsesBigModelEnvelope(t *testing.T) {
	// BigModel TOKENS_LIMIT window, unit 3 = 5h rate cap (Short), with the
	// currentValue/remaining/usage/nextResetTime fields ParseZhipuQuota reads.
	fixture := `{"success":true,"data":{"level":"tier-4","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":10,"usage":100000,"currentValue":10000,"remaining":90000,"nextResetTime":1750000000000}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("quota GET Authorization = %q, want Bearer k", got)
		}
		if got := r.Header.Get("x-api-key"); got != "k" {
			t.Errorf("quota GET x-api-key = %q, want k (ZCode sends both)", got)
		}
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	p := &ZCodeProvider{
		ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k"),
		cfg:        &Config{UsageURL: srv.URL},
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if s.Billing != BillingPlan {
		t.Fatalf("Billing = %v, want BillingPlan", s.Billing)
	}
	if len(s.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(s.Windows))
	}
	w := s.Windows[0]
	if !w.Short {
		t.Errorf("window not Short (unit 3 must be the 5h rate cap)")
	}
	if w.Duration != 5*time.Hour {
		t.Errorf("Duration = %v, want 5h", w.Duration)
	}
	if w.Used != 10000 || w.Total != 100000 {
		t.Errorf("Used/Total = %v/%v, want 10000/100000", w.Used, w.Total)
	}
}

func TestZCode_Quota_NonBigModelBody_IsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"unrelated":"body"}`))
	}))
	defer srv.Close()
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k"), cfg: &Config{UsageURL: srv.URL}}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota returned err: %v (must be nil)", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown for non-BigModel body", s.Billing)
	}
}
