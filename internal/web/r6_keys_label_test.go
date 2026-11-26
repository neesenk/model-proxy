package web

// r6_keys_label_test.go — transport contract of the key management endpoints
// (R6 F1): the service's classified rejection reaches the client verbatim —
// 400 with the exact error body — and the label reaches the service the way
// each handler promises (create trims, relabel forwards verbatim). The
// end-to-end guard over the real service, a real key file and the gate's
// credential set lives in the composition layer
// (internal/app/r6_keys_label_wiring_test.go).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"model-proxy/internal/appapi"
)

// r6KeyCommands scripts the two label-bearing commands; every other command
// is the shared fake.
type r6KeyCommands struct {
	*commandFake
	create  func(string) (appapi.APIKeyCreated, error)
	relabel func(string, string) error
}

func (f *r6KeyCommands) CreateAPIKey(label string) (appapi.APIKeyCreated, error) {
	return f.create(label)
}

func (f *r6KeyCommands) SetAPIKeyLabel(id, label string) error { return f.relabel(id, label) }

// r6KeysServer builds the transport over the scripted commands.
func r6KeysServer(t *testing.T, commands *r6KeyCommands) *Server {
	t.Helper()
	server, err := New(Options{
		Reads:    testReadAPI{},
		Commands: commands,
		Assets: fstest.MapFS{
			"assets/index.html": {Data: []byte("INDEX")},
			"assets/styles.css": {Data: []byte("CSS")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server
}

// r6LabelBody encodes the {label} request body (a label may carry line
// breaks, so the JSON is built, not spelled).
func r6LabelBody(t *testing.T, label string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"label": label})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestR6KeysSurfaceServiceRejection: when the service refuses a label, both
// endpoints answer 400 with exactly the service's error body.
func TestR6KeysSurfaceServiceRejection(t *testing.T) {
	const (
		controlChars = "label must not contain control characters"
		tooLong      = "label is longer than 64 characters"
	)
	cases := []struct {
		name      string
		path      string
		label     string
		wantLabel string
		wantErr   string
	}{
		{"create control characters", "/api/keys", "phone\nmp-key-0f0f", "phone\nmp-key-0f0f", controlChars},
		{"create too long", "/api/keys", " " + strings.Repeat("a", 65) + " ", strings.Repeat("a", 65), tooLong},
		{"relabel control characters", "/api/keys/one/label", "phone\r\nmp-key-0f0f", "phone\r\nmp-key-0f0f", controlChars},
		{"relabel too long", "/api/keys/one/label", " " + strings.Repeat("a", 65) + " ", " " + strings.Repeat("a", 65) + " ", tooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotID, gotLabel string
			rejected := appapi.NewHTTPError(http.StatusBadRequest, tc.wantErr)
			commands := &r6KeyCommands{
				commandFake: &commandFake{},
				create: func(label string) (appapi.APIKeyCreated, error) {
					gotLabel = label
					return appapi.APIKeyCreated{}, rejected
				},
				relabel: func(id, label string) error {
					gotID, gotLabel = id, label
					return rejected
				},
			}
			server := r6KeysServer(t, commands)
			requireCommandResponse(t, commandRequest(server, http.MethodPost, tc.path, r6LabelBody(t, tc.label)),
				http.StatusBadRequest, map[string]any{"error": tc.wantErr})
			// The transport is a thin decode/classify layer: the label reaches
			// the service as-is (create: trimmed), so the service owns the
			// validation and its message.
			if gotLabel != tc.wantLabel || (strings.HasPrefix(tc.path, "/api/keys/") && gotID != "one") {
				t.Errorf("service got id %q, label %q; want label %q", gotID, gotLabel, tc.wantLabel)
			}
		})
	}
}

// TestR6KeysMultilingualLabelPassesThrough: a label with spaces, Chinese and
// emoji crosses the transport untouched on both endpoints.
func TestR6KeysMultilingualLabelPassesThrough(t *testing.T) {
	// Fixed test data standing in for a minted key.
	const minted = "mp-key-0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"
	var gotLabel string
	commands := &r6KeyCommands{
		commandFake: &commandFake{},
		create: func(label string) (appapi.APIKeyCreated, error) {
			gotLabel = label
			// The DTO the real service builds for a minted key.
			return appapi.APIKeyCreated{
				APIKey: appapi.APIKey{ID: "1a2b3c4d", Label: label, Prefix: "mp-key-0f0f…"},
				Token:  minted,
			}, nil
		},
		relabel: func(id, label string) error {
			gotLabel = label
			return nil
		},
	}
	server := r6KeysServer(t, commands)

	requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/keys", r6LabelBody(t, "  手机 📱 平板  ")),
		http.StatusOK, map[string]any{
			"id": "1a2b3c4d", "label": "手机 📱 平板", "prefix": "mp-key-0f0f…",
			"requests_24h": float64(0), "token": minted,
		})
	if gotLabel != "手机 📱 平板" {
		t.Errorf("create reached the service with label %q, want the trimmed multilingual label", gotLabel)
	}
	requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/keys/one/label", r6LabelBody(t, "  中文 label 😀  ")),
		http.StatusOK, map[string]any{"id": "one", "status": "renamed"})
	if gotLabel != "  中文 label 😀  " {
		t.Errorf("relabel reached the service with label %q, want it verbatim", gotLabel)
	}
}
