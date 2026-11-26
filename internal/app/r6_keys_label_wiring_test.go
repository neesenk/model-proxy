package app

// r6_keys_label_wiring_test.go — composition-level guard against
// credential-file injection through the key management API (R6 F1): a label
// carrying line breaks must be refused at the HTTP boundary and leave the
// gate's credential set exactly as it was. The request travels the production
// path (web transport → admin service → key file), so the test belongs here;
// internal/web keeps only the transport contract over a scripted service.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/webauth"
)

// r6KeysInjectedKey is shaped like a forward key, so a label line that breaks
// onto its own line would be a working credential. Fixed test data.
const r6KeysInjectedKey = "mp-key-0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

// r6KeysSeeds are the credentials the seeded gate accepts.
var r6KeysSeeds = []string{"seed-key-0", "bare-key-1"}

// r6KeysWeb wires the production path over a seeded key file: proxy (with
// web.auth.api_keys_file) → web adapter → mux.
func r6KeysWeb(t *testing.T) (*WebServer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api_keys")
	if err := os.WriteFile(path, []byte("# @label seed\nseed-key-0\nbare-key-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configdomain.LoadConfigFromBytes("test", []byte(testConfigYAML("web:\n  auth:\n    api_keys_file: '"+path+"'\n")))
	if err != nil {
		t.Fatal(err)
	}
	loginAPIKeyFixtures(t, [2]string{"zhipu", "zhipu"})
	return NewWebServer(newTestProxy(t, cfg), "test-config.yaml"), path
}

// r6KeysPost dispatches one {label} request through the mux the WebServer
// registers on (production routing) and returns the recorder.
func r6KeysPost(t *testing.T, w *WebServer, path, label string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"label": label})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	serveWeb(w, recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
	return recorder
}

// r6KeysRequireResponse asserts an exact JSON response: the status, the JSON
// content type and precisely the given top-level fields with precisely the
// given values. A status mismatch is reported without the body — on a
// regression the body would carry a minted key.
func r6KeysRequireResponse(t *testing.T, recorder *httptest.ResponseRecorder, status int, want map[string]any) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d", recorder.Code, status)
	}
	if got := recorder.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON %q: %v", recorder.Body.String(), err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body = %v, want exactly %v", got, want)
	}
}

// r6KeysRequireGate pins the gate's credential set through the token-file
// reader the gate is built on, and that the injected string is not a key.
func r6KeysRequireGate(t *testing.T, path string, want ...string) {
	t.Helper()
	if got := webauth.ReadTokens(path); !reflect.DeepEqual(got, want) {
		// Count only: a minted key must not reach the test output.
		t.Errorf("gate reads %d credentials, want exactly the %d expected ones", len(got), len(want))
	}
	gate := webauth.NewSource(path)
	if gate.Accept(r6KeysInjectedKey) {
		t.Errorf("gate accepted the injected label line as a credential")
	}
	for _, token := range want {
		if !gate.Accept(token) {
			t.Errorf("gate no longer accepts an expected credential")
		}
	}
}

// r6KeysLabels lists the labels in file order — the token-free view used in
// failure messages.
func r6KeysLabels(entries []webauth.KeyEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Label)
	}
	return out
}

// TestR6APIKeysRejectUnsafeLabels: POST /api/keys and POST /api/keys/<id>/label
// answer 400 {"error": …} for a label with an interior line break (or beyond
// the label limit), and the gate still accepts exactly the two seed keys.
func TestR6APIKeysRejectUnsafeLabels(t *testing.T) {
	const (
		controlChars = "label must not contain control characters"
		tooLong      = "label is longer than 64 characters"
	)
	relabel := "/api/keys/" + webauth.KeyID("seed-key-0") + "/label"
	cases := []struct{ name, path, label, wantErr string }{
		{"create LF", "/api/keys", "phone\n" + r6KeysInjectedKey, controlChars},
		{"create CRLF", "/api/keys", "phone\r\n" + r6KeysInjectedKey, controlChars},
		{"create 65 runes", "/api/keys", strings.Repeat("a", 65), tooLong},
		{"relabel LF", relabel, "phone\n" + r6KeysInjectedKey, controlChars},
		{"relabel CRLF", relabel, "phone\r\n" + r6KeysInjectedKey, controlChars},
		{"relabel 65 runes", relabel, strings.Repeat("a", 65), tooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, path := r6KeysWeb(t)
			recorder := r6KeysPost(t, w, tc.path, tc.label)
			// Gate first: on a regression the credential set grows, and that
			// is the fact worth seeing next to the status mismatch.
			r6KeysRequireGate(t, path, r6KeysSeeds...)
			r6KeysRequireResponse(t, recorder, http.StatusBadRequest, map[string]any{"error": tc.wantErr})
		})
	}
}

// TestR6APIKeysAcceptMultilingualLabel: a label with spaces, Chinese and emoji
// is created (trimmed), relabeled and listed through the same wiring, and the
// gate grows by exactly the one minted key.
func TestR6APIKeysAcceptMultilingualLabel(t *testing.T) {
	w, path := r6KeysWeb(t)
	recorder := r6KeysPost(t, w, "/api/keys", "  手机 📱 平板  ")
	var created appapi.APIKeyCreated
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response (status %d): %v", recorder.Code, err)
	}
	if recorder.Code != http.StatusOK || created.Label != "手机 📱 平板" ||
		!strings.HasPrefix(created.Token, "mp-key-") || created.ID != webauth.KeyID(created.Token) {
		t.Fatalf("create = (%d, label %q)", recorder.Code, created.Label)
	}
	r6KeysRequireGate(t, path, "seed-key-0", "bare-key-1", created.Token)

	r6KeysRequireResponse(t, r6KeysPost(t, w, "/api/keys/"+created.ID+"/label", "中文 label 😀"),
		http.StatusOK, map[string]any{"id": created.ID, "status": "renamed"})
	// Padded label: the handler forwards it verbatim, the service trims it.
	r6KeysRequireResponse(t, r6KeysPost(t, w, "/api/keys/"+created.ID+"/label", "  phone  "),
		http.StatusOK, map[string]any{"id": created.ID, "status": "renamed"})
	if entries := webauth.ReadKeyEntries(path); len(entries) != 3 ||
		entries[2].Label != "phone" || entries[2].Token != created.Token {
		t.Errorf("key file holds %d entries, labels = %v", len(entries), r6KeysLabels(entries))
	}
	r6KeysRequireGate(t, path, "seed-key-0", "bare-key-1", created.Token)

	// The listing through the same wiring carries ids and labels in file
	// order, and never the full token.
	list := httptest.NewRecorder()
	serveWeb(w, list, httptest.NewRequest(http.MethodGet, "/api/keys", nil))
	var got appapi.KeysResult
	if err := json.Unmarshal(list.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode listing (status %d): %v", list.Code, err)
	}
	want := []appapi.APIKey{
		{ID: webauth.KeyID("seed-key-0"), Label: "seed"},
		{ID: webauth.KeyID("bare-key-1")},
		{ID: created.ID, Label: "phone"},
	}
	if list.Code != http.StatusOK || !got.Enabled || len(got.Keys) != len(want) {
		t.Fatalf("listing = (%d, enabled %v, %d keys), want 200 with %d", list.Code, got.Enabled, len(got.Keys), len(want))
	}
	for i := range want {
		if got.Keys[i].ID != want[i].ID || got.Keys[i].Label != want[i].Label {
			t.Errorf("listing key %d = (%q, %q), want (%q, %q)", i, got.Keys[i].ID, got.Keys[i].Label, want[i].ID, want[i].Label)
		}
	}
	if strings.Contains(list.Body.String(), created.Token) {
		t.Error("listing leaked the full token")
	}
}
