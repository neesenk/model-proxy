package app

import (
	"log"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// TestInitRequestLogArmsDiskGuard: with the guard armed, the request-log
// init line must state both conditions — the same values handed to the
// streams' logfile options, so an operator can see the guard is live.
func TestInitRequestLogArmsDiskGuard(t *testing.T) {
	var output syncLogBuffer
	captureAppLogs(t, &output)

	cfg := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{
		Enabled: true,
		DiskGuard: configdomain.DiskGuardConfig{
			Enabled:        true,
			MinFreeBytes:   50 << 30,
			MaxUsedPercent: 95,
		},
	}}
	proxy := newTestProxy(t, cfg)
	proxy.initRequestLog(cfg.RequestLog)

	logOutput := output.String()
	if !strings.Contains(logOutput, "disk guard min_free=53687091200 max_used=95%") {
		t.Errorf("enabled line missing the armed disk guard:\n%s", logOutput)
	}
}

// TestInitRequestLogSilentWhenGuardDisarmed: an explicit opt-out leaves the
// init line guard-free.
func TestInitRequestLogSilentWhenGuardDisarmed(t *testing.T) {
	var output syncLogBuffer
	captureAppLogs(t, &output)

	cfg := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{
		Enabled:   true,
		DiskGuard: configdomain.DiskGuardConfig{Enabled: false},
	}}
	proxy := newTestProxy(t, cfg)
	proxy.initRequestLog(cfg.RequestLog)

	if strings.Contains(output.String(), "disk guard") {
		t.Errorf("disarmed guard still announced:\n%s", output.String())
	}
}

// captureAppLogs routes the std log into buf for the test's duration.
func captureAppLogs(t *testing.T, buf *syncLogBuffer) {
	t.Helper()
	originalWriter := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(originalWriter) })
}
