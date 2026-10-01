package doctor_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/cli/clitest"
	clidoctor "model-proxy/internal/cli/doctor"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
)

// pooledRouteDownFixture builds the pooled route-down scene shared by the
// virtual-id tests: a zhipu pool with two accounts under an isolated HOME, one
// config route target on zhipu, and a daemon /api/status whose schedule shows
// the route fully down while health/model recovery horizons are keyed by the
// POOL VIRTUAL ids ("zhipu#<account>") — the keys the daemon's scheduler uses.
// It returns the rendered report and the sorted virtual ids.
func pooledRouteDownFixture(t *testing.T) (out string, vids []string) {
	t.Helper()
	home := t.TempDir()
	clitest.SetPoolHome(t, home)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "pool-key-1", "pool-key-2")

	quotaUntil := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	circuitUntil := time.Now().Add(45 * time.Minute).UTC().Format(time.RFC3339)

	// The health keys depend on the pool's virtual ids, so the config must load
	// (for PoolVirtuals) before the status JSON can name them. A handler that
	// reads the keys lazily keeps the fixture in one place.
	var cfg *configdomain.Config
	statusJSON := func(addr string) string {
		vids, _ = climodels.PoolVirtuals(cfg, "zhipu")
		if len(vids) != 2 {
			t.Fatalf("PoolVirtuals = %v, want 2 virtual ids (pool not resolved — HOME seam broken?)", vids)
		}
		return fmt.Sprintf(`{
		  "uptime":"1m0s","version":"0.4.2","listen":%q,
		  "health":{
		    %q:{"circuit_state":"closed","available":false,"rate_limited_until":%q,"rate_limit_kind":"quota"},
		    %q:{"circuit_state":"open","available":false,"circuit_until":%q}
		  },
		  "quota":{},
		  "schedule":{"models":{"claude":{"first":"","ordered":[]}}},
		  "model_locks":{}
		}`, addr, vids[0], quotaUntil, vids[1], circuitUntil)
	}
	addr := doctorLiveTestServer(t, statusJSON, `{"enabled":false,"records":[]}`)
	cfg = doctorLiveTestCfg(t, addr, `providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  claude:
    - {provider: zhipu, model: glm-5.2, priority: 1}
`)

	out, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("renderDoctorLive: %v", err)
	}
	return out, vids
}

// TestRenderDoctorLivePooledRouteDownVirtualIDs pins LiveTargets' pool
// expansion (live.go): a route with ONE config target on a pooled provider
// must be diagnosed over the pool's virtual accounts — the target count says
// 2 (not 1), and the earliest-recovery scan must match /api/status entries
// keyed by the virtual ids (a bare "zhipu" key would never match and the
// recovery line would vanish).
func TestRenderDoctorLivePooledRouteDownVirtualIDs(t *testing.T) {
	out, vids := pooledRouteDownFixture(t)

	if !strings.Contains(out, `✗ route "claude": 2 targets all unavailable`) {
		t.Errorf("pooled route-down must expand 1 config target to 2 pool virtuals:\n%s", out)
	}
	// The earliest horizon is vids[0]'s 20m quota cooldown (vids[1]'s circuit
	// opens later). Matching it proves the health lookup went through the
	// virtual id keys.
	quotaUntil := time.Now().Add(20 * time.Minute).UTC()
	if !strings.Contains(out, "earliest recovery "+quotaUntil.Local().Format("15:04")+" ("+vids[0]+", quota cooldown)") {
		t.Errorf("earliest recovery must resolve through the virtual-id health key %q:\n%s", vids[0], out)
	}
	if strings.Contains(out, "circuit breaker)") {
		t.Errorf("earliest recovery should pick the 20m quota cooldown, not the 45m circuit:\n%s", out)
	}
}

// TestRenderDoctorLiveHintsUsePoolParentName pins poolParent (live.go): the
// action hints must name the config-level POOL PARENT (the identifier
// login/unfreeze take), never the virtual account id — CLI.md §12: "建议里的
// provider 一律用池父名（虚拟 id `parent#<account>` 归一到 `parent`）".
func TestRenderDoctorLiveHintsUsePoolParentName(t *testing.T) {
	out, _ := pooledRouteDownFixture(t)

	for _, want := range []string{
		"[可立即执行] model-proxy unfreeze zhipu，或等冷却到期自动恢复",
		// zhipu is pool-capable and the cause is a quota cooldown -> the
		// account-pool login remedy, addressed to the parent name.
		"[需要凭据] model-proxy login zhipu（账号池 provider 可再加一个账号分担配额）",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hint missing parent-name form %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unfreeze zhipu#") || strings.Contains(out, "login zhipu#") {
		t.Errorf("hints must address the pool parent, never a virtual account id:\n%s", out)
	}
}

// TestRenderDoctorLiveHTTPError: a non-200 /api/status surfaces the daemon
// error (status + truncated body), mirroring serve status — same failure
// family CLI.md §12 documents as "同 §10 的 4 类".
func TestRenderDoctorLiveHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()
	cfg := doctorLiveTestCfg(t, ts.Listener.Addr().String(), "providers:\n  aqp: {provider_id: aqp, openai_base_url: https://x}\n")
	_, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "daemon returned HTTP 500: boom") {
		t.Fatalf("want HTTP 500 error with body, got %v", err)
	}
}

// TestRenderDoctorLiveParseError: a 200 with a non-JSON body fails loudly
// instead of rendering an empty diagnosis.
func TestRenderDoctorLiveParseError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not-json{"))
	}))
	defer ts.Close()
	cfg := doctorLiveTestCfg(t, ts.Listener.Addr().String(), "providers:\n  aqp: {provider_id: aqp, openai_base_url: https://x}\n")
	_, err := clidoctor.RenderDoctorLive(cfg, filepath.Join(t.TempDir(), "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "parse status response") {
		t.Fatalf("want parse error, got %v", err)
	}
}
