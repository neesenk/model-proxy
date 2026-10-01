package doctor

import (
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"

	configdomain "model-proxy/internal/config"
)

// --- doctor ---

// TestDoctorFusion: the doctor diagnostic renders a Fusion section with the
// panel/quorum plus the cost and quality knobs.
func TestDoctorFusion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := configdomain.LoadConfigFromBytes("config.yaml", []byte(`listen: 127.0.0.1:15721
providers:
  a: {openai_base_url: "https://a", provider_id: static}
  b: {openai_base_url: "https://b", provider_id: static}
  s: {openai_base_url: "https://s", provider_id: static}
  j: {openai_base_url: "https://j", provider_id: static}
fusion:
  r:
    panel:
      - {provider: a, model: ma}
      - {provider: b, model: mb}
    synthesizer: {provider: s, model: ms}
    max_runs_per_day: 50
    first_turn_only: true
    judge: {provider: j, model: mj}
    instruction: "自定义指令"
routes:
  hard:
    - {provider: fusion, model: r, priority: 1}
`))
	if err != nil {
		t.Fatalf("config rejected: %v", err)
	}
	out := clitest.GrabStdout(t, func() { DoctorWithCfg(cfg) })
	for _, want := range []string{"Fusion", "panel=2", "quorum=2", "synthesizer=s/ms", "budget=50/day", "first_turn_only", "judge=j/mj", "custom instruction"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}
