package status

import (
	"model-proxy/internal/cli/clitest"
	"strings"
	"testing"
)

const routesCmdConfig = `listen: 127.0.0.1:15721
providers:
  kimi-code:
    provider_id: kimi-code
    openai_base_url: https://kc.example/v1
    priority: 1
    alias: {k3: kimi-k3}
    models: [k3, kimi-for-coding]
  aqp:
    provider_id: aqp
    openai_base_url: https://aqp.example/v1
    priority: 2
    models: [kimi-k3]
  deepseek:
    provider_id: deepseek
    openai_base_url: https://ds.example/v1
    priority: 4
    billing: pay-as-you-go
    models: [deepseek-v4-pro]
`

func TestCLI_RoutesList(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, routesCmdConfig)
	stdout, _, code := clitest.RunCLI(t, "routes", cfgPath)
	if code != 0 {
		t.Fatalf("routes exit=%d stderr-missing", code)
	}
	if !strings.Contains(stdout, "kimi-k3") || !strings.Contains(stdout, "kimi-code/k3") || !strings.Contains(stdout, "aqp/kimi-k3") {
		t.Errorf("route list should show the aggregated kimi-k3 row with real upstream names:\n%s", stdout)
	}
	if !strings.Contains(stdout, "kimi-for-coding") {
		t.Errorf("route list should include kimi-for-coding:\n%s", stdout)
	}
	if i, j := strings.Index(stdout, "kimi-code/k3"), strings.Index(stdout, "aqp/kimi-k3"); !(i >= 0 && j > i) {
		t.Errorf("targets must be ordered by priority (kimi-code before aqp):\n%s", stdout)
	}
}

func TestCLI_RoutesDetail(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, routesCmdConfig)
	stdout, _, code := clitest.RunCLI(t, "routes", cfgPath, "kimi-k3")
	if code != 0 {
		t.Fatalf("routes kimi-k3 exit=%d", code)
	}
	for _, want := range []string{"route kimi-k3", "alias of kimi-code/k3", "kimi-code", "model=k3", "priority=1", "aqp", "priority=2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("detail output missing %q:\n%s", want, stdout)
		}
	}

	stdout, _, _ = clitest.RunCLI(t, "routes", cfgPath, "deepseek-v4-pro")
	if !strings.Contains(stdout, "billing=pay-as-you-go") {
		t.Errorf("detail should show the configured billing method:\n%s", stdout)
	}
}

func TestCLI_RoutesUnknownModel(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, routesCmdConfig)
	_, stderr, code := clitest.RunCLI(t, "routes", cfgPath, "nope")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no route for model \"nope\"") || !strings.Contains(stderr, "kimi-k3") {
		t.Errorf("stderr should name the model and the available routes:\n%s", stderr)
	}
}
