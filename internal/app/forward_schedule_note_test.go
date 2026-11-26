package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
)

// TestForward_RequestAwareRerouteLogsScheduleNote: request-aware routing must
// leave a trace — the per-target fit verdicts ride both the x-mp-reroute
// response header and the request log's schedule.note
// (docs/architecture/routing-and-failure.md). The note reached the header and
// the all-targets-failed terminal row, but the committed row — the row an
// answered request actually gets — dropped it: forward projected only the
// scheduling source into the executor's log context. A request that fits
// every target is the control: no header, no note.
func TestForward_RequestAwareRerouteLogsScheduleNote(t *testing.T) {
	var textHit bool
	textUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		textHit = true
		w.Write([]byte(`{}`))
	}))
	defer textUp.Close()
	visionUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer visionUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"text-p":   {OpenAIBaseURL: textUp.URL, Provider: testProviderID},
			"vision-p": {OpenAIBaseURL: visionUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {
				{Provider: "text-p", Model: "text", Priority: 1},
				{Provider: "vision-p", Model: "vision", Priority: 2},
			},
		},
	}
	proxy, dir, shutdown := newReqLogProxy(t, cfg)
	proxy.providers["text-p"] = &testProv{key: "t"}
	proxy.providers["vision-p"] = &testProv{key: "v"}
	proxy.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"text":   {Context: 8000, Input: []string{"text"}},
		"vision": {Context: 8000, Input: []string{"text", "image"}},
	})
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	post := func(body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatalf("read response: %v", err)
		}
		return resp
	}

	// An image request: the text-only priority-1 target does not fit and is
	// dropped — the reroute the note must describe.
	const imageBody = `{"model":"glm","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64"}}]}]}`
	imageResp := post(imageBody)
	if imageResp.StatusCode != 200 || textHit {
		t.Fatalf("image request: status=%d textHit=%v, want 200 answered by the vision target only", imageResp.StatusCode, textHit)
	}
	headerNote := imageResp.Header.Get("x-mp-reroute")
	if !strings.HasPrefix(headerNote, "request-aware: dropped text-p/text: ") {
		t.Fatalf("x-mp-reroute = %q, want the fit verdict of the dropped target", headerNote)
	}

	// Control: a text request fits both targets, so nothing is rerouted.
	const textBody = `{"model":"glm","messages":[{"role":"user","content":"hi"}]}`
	textResp := post(textBody)
	if textResp.StatusCode != 200 {
		t.Fatalf("text request: status=%d, want 200", textResp.StatusCode)
	}
	if note := textResp.Header.Get("x-mp-reroute"); note != "" {
		t.Fatalf("text request carries x-mp-reroute = %q, want none", note)
	}

	// Each answered row is completed at response-body Close inside the
	// post-copy commit path — a Content-Length client can finish reading
	// first, and draining before that drops the row. Wait for the commit
	// metrics (recorded right after) so the drain below flushes both rows.
	awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "vision-p", Model: "vision"})
	awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "text-p", Model: "text"})
	shutdown()
	records := allRecords(t, dir)
	if len(records) != 2 {
		t.Fatalf("records = %d, want the two committed rows", len(records))
	}
	for _, record := range records {
		if record.Schedule == nil {
			t.Fatalf("record %q has no schedule decision", record.RequestBody)
		}
		want := ""
		if record.RequestBody == imageBody {
			want = headerNote
		}
		if record.Schedule.Note != want {
			t.Errorf("record %q schedule.note = %q, want %q (the same trace as the response header)", record.RequestBody, record.Schedule.Note, want)
		}
	}
}
