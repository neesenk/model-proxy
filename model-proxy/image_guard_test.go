package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sonic "github.com/bytedance/sonic"
)

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

func TestShrinkRequestImagesBoundsDimensions(t *testing.T) {
	url := testPNGDataURL(t, 3000, 2)
	body := []byte(`{"image_url":"` + url + `"}`)
	out, changed := shrinkRequestImages(body, 1<<20, 2048)
	if !changed {
		t.Fatal("expected oversized dimensions to be normalized")
	}
	root := unmarshalMap(t, out)
	_, data, ok := parseDataURL(strOpt(root["image_url"]))
	if !ok {
		t.Fatalf("output is not an image data URL: %s", out)
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width > 2048 || cfg.Height > 2048 || len(raw) > 1<<20 {
		t.Fatalf("guard output = %dx%d, %d bytes", cfg.Width, cfg.Height, len(raw))
	}
}

// A rewritten image forces a full body re-marshal; >2^53 integer literals
// (snowflake-style ids) elsewhere in the body must survive that round trip.
func TestImageGuard_PreservesBigIntegersOnRewrite(t *testing.T) {
	url := testPNGDataURL(t, 3000, 2)
	body := []byte(`{"snowflake_id":9007199254740993,"nested":{"ids":[2535301200456458802,-9007199254740993]},"image_url":"` + url + `"}`)
	out, changed := shrinkRequestImages(body, 1<<20, 2048)
	if !changed {
		t.Fatal("expected oversized image to be rewritten")
	}
	wire := string(out)
	for _, literal := range []string{"9007199254740993", "2535301200456458802", "-9007199254740993"} {
		if !strings.Contains(wire, literal) {
			t.Errorf("rewritten body lost integer literal %s: %s", literal, wire)
		}
	}

	// Untouched bodies (no rewrite) stay byte-identical.
	plain := []byte(`{"snowflake_id":9007199254740993,"message":"no image"}`)
	got, ok := shrinkRequestImages(plain, 1<<20, 2048)
	if ok || !bytes.Equal(got, plain) {
		t.Fatalf("no-image body must stay byte-identical: %s, changed=%v", got, ok)
	}
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
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
