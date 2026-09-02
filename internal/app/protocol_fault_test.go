package app

import (
	"bytes"
	"encoding/base64"
	sonic "github.com/bytedance/sonic"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- protocol_fault_integration_test.go ----

func TestConvertFault_SameProtocolPassthrough(t *testing.T) {
	antBody := `{"model":"claude-x","max_tokens":100,"system":"s","messages":[{"role":"user","content":"hi"}]}`
	antResp := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"yo"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	var gotAnt string
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("anthropic upstream path = %q", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotAnt = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(antResp))
	}))
	defer upA.Close()
	pA := newTestProxy(t, &Config{
		Providers: map[string]Provider{"ant": {AnthropicBaseURL: upA.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "ant", Model: "claude-x"}}},
	})
	pA.providers["ant"] = &testProv{key: "k"}
	pxA := httptest.NewServer(http.HandlerFunc(pA.Handler))
	defer pxA.Close()
	resp, err := http.Post(pxA.URL+"/v1/messages", "application/json", strings.NewReader(antBody))
	if err != nil {
		t.Fatal(err)
	}
	bodyA, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if gotAnt != antBody {
		t.Errorf("anthropic request not byte-identical: got %s want %s", gotAnt, antBody)
	}
	if string(bodyA) != antResp {
		t.Errorf("anthropic response not byte-identical: got %s want %s", bodyA, antResp)
	}

	rspBody := `{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	rspResp := `{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	var gotRsp string
	upR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("responses upstream path = %q", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotRsp = string(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(rspResp))
	}))
	defer upR.Close()
	pR := newTestProxy(t, &Config{
		Providers: map[string]Provider{"cdx": {OpenAIBaseURL: upR.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "cdx", Model: "gpt-x"}}},
	})
	pR.providers["cdx"] = &testProv{key: "k"}
	pxR := httptest.NewServer(http.HandlerFunc(pR.Handler))
	defer pxR.Close()
	resp2, err := http.Post(pxR.URL+"/v1/responses", "application/json", strings.NewReader(rspBody))
	if err != nil {
		t.Fatal(err)
	}
	bodyR, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if gotRsp != rspBody {
		t.Errorf("responses request not byte-identical: got %s want %s", gotRsp, rspBody)
	}
	if string(bodyR) != rspResp {
		t.Errorf("responses response not byte-identical: got %s want %s", bodyR, rspResp)
	}
}

type zeroReader struct{ b byte }

func (z zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = z.b
	}
	return len(p), nil
}

func TestConvertFault_NonStream64MiBCap(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(200)
		io.CopyN(w, zeroReader{b: 'x'}, 65<<20)
	}))
	defer up.Close()
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	})
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if bytes.Contains(body, bytes.Repeat([]byte{'x'}, 1<<20)) {
		t.Errorf("client received raw upstream bytes")
	}
}

func TestConvertFault_DisconnectStopsUpstream(t *testing.T) {
	closed := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk0\"}}]}\n\n")
		f.Flush()
		for {
			select {
			case <-r.Context().Done():
				closed <- struct{}{}
				return
			case <-time.After(10 * time.Millisecond):
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
				f.Flush()
			}
		}
	}))
	defer up.Close()
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	})
	p.providers["oai"] = &testProv{key: "k"}
	rec := &disconnectWriter{ResponseRecorder: httptest.NewRecorder()}
	p.Handler(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Error("upstream connection not closed within 3s")
	}
}

func TestConvertFault_SniffSSEMissingContentType(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain")
		w.WriteHeader(200)
		io.WriteString(w, "event: response.created\n"+
			`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`+"\n\n"+
			"event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n\n")
	}))
	defer up.Close()
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"cdx": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "cdx", Model: "gpt-x", Protocol: "responses"}}},
	})
	p.providers["cdx"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	for _, want := range []string{"event: message_start", `"text":"hi"`, "event: message_stop"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("sniffed stream missing %q:\n%s", want, body)
		}
	}
}

// ---- protocol_image_guard_integration_test.go ----

func testPNGDataURL(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		img.Set(x, x%height, color.RGBA{R: uint8(x), G: 80, B: 160, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestForwardRetries413AfterImageCompression(t *testing.T) {
	url := testPNGDataURL(t, 3000, 2)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if calls.Add(1) == 1 {
			http.Error(w, `{"error":{"message":"too large"}}`, http.StatusRequestEntityTooLarge)
			return
		}
		var request map[string]any
		if err := sonic.Unmarshal(body, &request); err != nil {
			t.Errorf("retry body: %v", err)
		}
		wire := string(body)
		start := strings.Index(wire, "data:image/jpeg;base64,")
		if start < 0 {
			t.Errorf("retry body did not contain compressed JPEG: %s", body)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"ok","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	request := `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
		strings.TrimPrefix(url, "data:image/png;base64,") + `"}}]}]}`
	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}
