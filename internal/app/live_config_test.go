package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveConfigPathDefaultsToRepositoryConfig(t *testing.T) {
	t.Setenv(liveConfigEnv, "")

	got, err := liveConfigPath()
	if err != nil {
		t.Fatalf("liveConfigPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("liveConfigPath = %q, want absolute path", got)
	}
	if filepath.Base(got) != "config.yaml" {
		t.Fatalf("liveConfigPath = %q, want repository config.yaml", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(got), "go.mod")); err != nil {
		t.Fatalf("liveConfigPath = %q, parent is not repository root: %v", got, err)
	}
}

func TestLiveConfigPathOverride(t *testing.T) {
	t.Run("absolute", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "live.yaml")
		t.Setenv(liveConfigEnv, want)

		got, err := liveConfigPath()
		if err != nil {
			t.Fatalf("liveConfigPath: %v", err)
		}
		if got != want {
			t.Fatalf("liveConfigPath = %q, want %q", got, want)
		}
	})

	t.Run("repository-relative", func(t *testing.T) {
		const override = "testdata/live.yaml"
		t.Setenv(liveConfigEnv, override)

		root, err := liveRepositoryRoot()
		if err != nil {
			t.Fatalf("liveRepositoryRoot: %v", err)
		}
		got, err := liveConfigPath()
		if err != nil {
			t.Fatalf("liveConfigPath: %v", err)
		}
		want := filepath.Join(root, override)
		if got != want {
			t.Fatalf("liveConfigPath = %q, want %q", got, want)
		}
	})
}

func TestValidateLiveResponsesCompleted(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name: "single completed",
			raw:  "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
		},
		{
			name:    "missing terminal",
			raw:     "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/0/0",
		},
		{
			name:    "incomplete",
			raw:     "event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/1/0",
		},
		{
			name: "completed and incomplete",
			raw: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n" +
				"event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n",
			wantErr: "completed/incomplete/failed = 1/1/0",
		},
		{
			name: "duplicate completed",
			raw: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "completed/incomplete/failed = 2/0/0",
		},
		{
			name:    "completed frame missing response",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "missing nested response object",
		},
		{
			name:    "completed frame missing status",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"error\":null}}\n\n",
			wantErr: "missing nested response.status",
		},
		{
			name:    "failed",
			raw:     "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/0/1",
		},
		{
			name:    "cancelled",
			raw:     "event: response.cancelled\ndata: {\"type\":\"response.cancelled\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/0/1",
		},
		{
			name:    "completed frame with failed status",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n",
			wantErr: "nested status = \"failed\"",
		},
		{
			name:    "completed frame with cancelled status",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"cancelled\"}}\n\n",
			wantErr: "nested status = \"cancelled\"",
		},
		{
			name:    "completed frame with nested error",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":{\"message\":\"bad\"}}}\n\n",
			wantErr: "nested response.error",
		},
		{
			name: "error event despite completion",
			raw: "event: error\ndata: {\"type\":\"error\",\"message\":\"bad\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "unexpected SSE error event",
		},
		{
			name: "error payload despite completion",
			raw: "event: response.output_text.delta\ndata: {\"error\":{\"message\":\"bad\"}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "unexpected SSE error payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLiveResponsesCompleted(parseSSE(tt.raw))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateLiveResponsesCompleted: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLiveResponsesCompleted error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateLiveWeatherCall(t *testing.T) {
	tests := []struct {
		name               string
		callID, tool, args string
		wantErr            string
	}{
		{name: "valid", callID: "call-1", tool: "get_weather", args: `{"city":"Paris"}`},
		{name: "missing id", tool: "get_weather", args: `{"city":"Paris"}`, wantErr: "id is empty"},
		{name: "missing name", callID: "call-1", args: `{"city":"Paris"}`, wantErr: "name is empty"},
		{name: "missing arguments", callID: "call-1", tool: "get_weather", wantErr: "arguments are empty"},
		{name: "wrong name", callID: "call-1", tool: "other", args: `{"city":"Paris"}`, wantErr: "want get_weather"},
		{name: "invalid JSON", callID: "call-1", tool: "get_weather", args: `{"city":`, wantErr: "valid JSON object"},
		{name: "JSON array", callID: "call-1", tool: "get_weather", args: `["Paris"]`, wantErr: "valid JSON object"},
		{name: "missing city", callID: "call-1", tool: "get_weather", args: `{}`, wantErr: "want Paris"},
		{name: "wrong city", callID: "call-1", tool: "get_weather", args: `{"city":"London"}`, wantErr: "want Paris"},
		{name: "wrong city case", callID: "call-1", tool: "get_weather", args: `{"city":"paris"}`, wantErr: "want Paris"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLiveWeatherCall(tt.callID, tt.tool, tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateLiveWeatherCall: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLiveWeatherCall error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}
