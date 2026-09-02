package stats

import (
	"model-proxy/internal/cli/clitest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCLI_UsageSingleAccount covers the composition-root branch that turns a
// one-account credential pool into a plain provider implementation and
// dispatches Usage with that account's bound credential. Provider-specific
// fetch, parse, and display behavior belongs to provider/*_test.go.
func TestCLI_UsageSingleAccount(t *testing.T) {
	seenAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[` +
			`{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1750000000000,` +
			`"usage":200000,"currentValue":140000,"remaining":60000}]}}`))
	}))
	defer srv.Close()

	clitest.SetPoolHome(t, t.TempDir())
	clitest.WritePoolFile(t, "zhipu", "zhipu", "SINGLE-KEY")
	cfgPath := clitest.WriteZhipuPoolConfig(t, srv.URL)

	out := clitest.GrabStdout(t, func() {
		RunUsage([]string{"zhipu", "--config", cfgPath})
	})

	select {
	case got := <-seenAuth:
		if got != "Bearer SINGLE-KEY" {
			t.Fatalf("Authorization=%q, want %q", got, "Bearer SINGLE-KEY")
		}
	default:
		t.Fatal("single-account usage did not call the provider")
	}
	for _, want := range []string{"zhipu", "GLM Coding Plan", "Weekly tokens"} {
		if !strings.Contains(out, want) {
			t.Errorf("single-account usage missing %q:\n%s", want, out)
		}
	}
}
