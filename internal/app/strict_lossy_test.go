package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestForward_StrictLossyRefusesAndAnswers400: with conversion.strict_lossy
// on, a request whose conversion is lossy-but-degradable is refused by every
// converting target and the client gets the 400 unsupported envelope (no
// silent degradation); with strict off the same request converts and 200s.
func TestForward_StrictLossyRefusesAndAnswers400(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	newProxy := func(strict bool) *httptest.Server {
		cfg := &Config{
			Providers: map[string]Provider{"backend": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
			Routes: map[string][]RouteTarget{
				"glm": {{Provider: "backend", Model: "backend-model", Protocol: "responses"}},
			},
			Conversion: ConversionConfig{StrictLossy: strict},
		}
		p := newTestProxy(t, cfg)
		p.providers["backend"] = &testProv{key: "k"}
		px := httptest.NewServer(http.HandlerFunc(p.Handler))
		t.Cleanup(px.Close)
		return px
	}
	// anthropic client → responses backend with `stop` (no Responses
	// equivalent → stop_dropped diagnostic).
	body := `{"model":"glm","max_tokens":16,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`

	resp, err := http.Post(newProxy(false).URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("strict off: status = %d, want 200 (lossy tolerated)", resp.StatusCode)
	}

	resp, err = http.Post(newProxy(true).URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("strict on: status = %d, want 400: %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("400 body not an anthropic error envelope: %s", raw)
	}
	if !strings.Contains(string(raw), "strict_lossy") {
		t.Fatalf("400 body lacks the strict_lossy feature marker: %s", raw)
	}
}
