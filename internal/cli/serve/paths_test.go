package serve_test

import (
	"os"
	"path/filepath"
	"testing"

	serve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
)

func TestResolveLogFile(t *testing.T) {
	cfg := &configdomain.Config{LogFile: "/from/config.log"}
	sa := serve.Args{Config: "/etc/model-proxy/config.yaml"}

	// config used
	if got := serve.ResolveLogFile(sa, cfg); got != "/from/config.log" {
		t.Errorf("config: got %q", got)
	}
	// default: the OS temp dir (runtime artifacts), e.g. /tmp on Linux, $TMPDIR on macOS
	wantDefault := filepath.Join(os.TempDir(), "model-proxy.log")
	if got := serve.ResolveLogFile(serve.Args{Config: "/etc/model-proxy/config.yaml"}, &configdomain.Config{}); got != wantDefault {
		t.Errorf("default: got %q want %q", got, wantDefault)
	}
}

func TestPidFilePath(t *testing.T) {
	if got := serve.PidFilePath("/var/log/model-proxy.log"); got != "/var/log/model-proxy.pid" {
		t.Errorf("got %q", got)
	}
}
