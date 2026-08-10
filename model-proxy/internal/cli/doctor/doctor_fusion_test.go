package doctor

import (
	"io"
	"os"
	"strings"
	"testing"

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
	out := captureStdout(t, func() { DoctorWithCfg(cfg) })
	for _, want := range []string{"Fusion", "panel=2", "quorum=2", "synthesizer=s/ms", "budget=50/day", "first_turn_only", "judge=j/mj", "custom instruction"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

// captureStdout captures os.Stdout during fn.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}
