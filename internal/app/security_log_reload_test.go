package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"model-proxy/internal/observe/seclog"
)

// Security audit log as reload-owned state (security_log_adapter.go's
// reconcileSecLog): audit off→on starts logging at reload, on→off stops it,
// an audit_path change swaps files, and Close drains the current generation.
// Fixtures are synthetic (guardPoolKey matches no embedded rule).

// seclogRig wires a pool-backed provider behind a YAML config file so tests
// can Reload between audit configurations. reconcileSecLog stands in for
// StartRuntimeServices' boot reconcile (tests never call StartRuntimeServices
// — it would open stats/request-log/catalog services).
type seclogRig struct {
	proxy   *Proxy
	url     string
	upURL   string
	cfgPath string
}

func newSecLogRig(t *testing.T, audit bool, auditPath string) *seclogRig {
	t.Helper()
	setPoolHome(t, t.TempDir())
	writePoolFile(t, "zhipu", "zhipu", guardPoolKey)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	rig := &seclogRig{upURL: up.URL, cfgPath: filepath.Join(t.TempDir(), "config.yaml")}
	rig.writeConfig(t, audit, auditPath)
	rig.proxy = newTestProxy(t, mustLoadConfigFile(t, rig.cfgPath))
	// Boot reconcile (StartRuntimeServices does this in production).
	rig.proxy.reconcileSecLog(rig.proxy.cfgSnapshot())
	px := httptest.NewServer(http.HandlerFunc(rig.proxy.Handler))
	t.Cleanup(px.Close)
	rig.url = px.URL
	return rig
}

// writeConfig rewrites the rig's YAML with the given audit settings.
func (r *seclogRig) writeConfig(t *testing.T, audit bool, auditPath string) {
	t.Helper()
	if err := os.WriteFile(r.cfgPath, []byte(r.configYAML(audit, auditPath)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// configYAML renders the rig's config for the given audit settings.
func (r *seclogRig) configYAML(audit bool, auditPath string) string {
	yaml := "listen: 127.0.0.1:0\n" +
		"providers:\n  zhipu:\n    openai_base_url: " + r.upURL + "\n    provider_id: zhipu\n" +
		"routes:\n  glm:\n    - {provider: zhipu, model: glm}\n" +
		"guard:\n  secrets: log\n"
	if audit {
		yaml += "  audit: true\n"
		if auditPath != "" {
			yaml += "  audit_path: " + auditPath + "\n"
		}
	} else {
		yaml += "  audit: false\n"
	}
	return yaml
}

// reload swaps the rig's config generation to the given audit settings.
func (r *seclogRig) reload(t *testing.T, audit bool, auditPath string) {
	t.Helper()
	r.writeConfig(t, audit, auditPath)
	if err := r.proxy.Reload(r.cfgPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

// guardHit posts one body that hits the known-secret channel.
func (r *seclogRig) guardHit(t *testing.T) {
	t.Helper()
	postOK(t, r.url+"/v1/chat/completions", guardPoolRequestBody(guardPoolKey))
}

// reloadErr and guardHitErr are the goroutine-safe variants of reload/guardHit:
// they return errors instead of calling t.Fatal, which is only legal on the
// test goroutine.
func (r *seclogRig) reloadErr(audit bool, auditPath string) error {
	if err := os.WriteFile(r.cfgPath, []byte(r.configYAML(audit, auditPath)), 0o600); err != nil {
		return err
	}
	return r.proxy.Reload(r.cfgPath)
}

func (r *seclogRig) guardHitErr() error {
	resp, err := http.Post(r.url+"/v1/chat/completions", "application/json", stringReader(guardPoolRequestBody(guardPoolKey)))
	if err != nil {
		return err
	}
	b, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("guard hit: status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

// seclogRecordCount returns how many audit records dir holds (0 if absent).
func seclogRecordCount(t *testing.T, dir string) int {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		return 0
	}
	result, err := seclog.Query(dir, seclog.Filter{})
	if err != nil {
		t.Fatalf("query %s: %v", dir, err)
	}
	return len(result.Records)
}

// audit off→on via reload takes effect immediately: hits before the reload
// persist nothing; the first hit after it lands in the audit log.
func TestSecLogReload_AuditOffToOn(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1") // fail catalog refresh fast
	dir := filepath.Join(t.TempDir(), "audit")
	rig := newSecLogRig(t, false, "")

	rig.guardHit(t)
	if got := seclogRecordCount(t, dir); got != 0 {
		t.Fatalf("audit off: records = %d, want 0", got)
	}

	rig.reload(t, true, filepath.Join(dir, "security.log"))
	if rig.proxy.SnapshotRuntime().SecLog == nil {
		t.Fatal("post-reload: snapshot must carry an audit logger (off→on)")
	}

	rig.guardHit(t)
	waitUntil(t, "first audit record after off→on reload", func() bool {
		return seclogRecordCount(t, dir) >= 1
	})
	result, err := seclog.Query(dir, seclog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Records[0]; got.Kind != seclog.KindSecret || got.Action != "log" {
		t.Errorf("first record = %+v, want kind=secret action=log", got)
	}
}

// audit on→off via reload stops new records at once: the old logger is drained
// during the reload, and post-reload hits write nothing.
func TestSecLogReload_AuditOnToOff(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dir := filepath.Join(t.TempDir(), "audit")
	rig := newSecLogRig(t, true, filepath.Join(dir, "security.log"))

	rig.guardHit(t)
	waitUntil(t, "audit record before the off reload", func() bool {
		return seclogRecordCount(t, dir) >= 1
	})
	before := seclogRecordCount(t, dir)

	rig.reload(t, false, "")
	if rig.proxy.SnapshotRuntime().SecLog != nil {
		t.Fatal("post-reload: snapshot logger must be nil (on→off)")
	}

	// Deterministic: the old logger was drained+stopped synchronously during
	// Reload, and the new generation's snapshot carries no logger.
	rig.guardHit(t)
	if got := seclogRecordCount(t, dir); got != before {
		t.Errorf("post-reload records = %d, want unchanged %d (on→off must stop writes)", got, before)
	}
}

// An audit_path change swaps files: new records land in the new directory and
// the old file is closed (its count never moves again).
func TestSecLogReload_AuditPathChange(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dirA := filepath.Join(t.TempDir(), "audit-a")
	dirB := filepath.Join(t.TempDir(), "audit-b")
	rig := newSecLogRig(t, true, filepath.Join(dirA, "security.log"))

	rig.guardHit(t)
	waitUntil(t, "audit record in the original directory", func() bool {
		return seclogRecordCount(t, dirA) >= 1
	})

	rig.reload(t, true, filepath.Join(dirB, "security.log"))
	logger := rig.proxy.SnapshotRuntime().SecLog
	if logger == nil || logger.Directory() != dirB {
		t.Fatalf("post-reload logger dir = %v, want %s", logger, dirB)
	}

	rig.guardHit(t)
	waitUntil(t, "audit record in the new directory", func() bool {
		return seclogRecordCount(t, dirB) >= 1
	})
	if got := seclogRecordCount(t, dirA); got != 1 {
		t.Errorf("old directory records = %d, want 1 (old file closed at swap)", got)
	}
}

// Reloads toggling audit racing concurrent guard hits: no data race (race
// detector), no deadlock, and the final generation converges.
func TestSecLogReload_ConcurrentHitsAndReloads(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dirA := filepath.Join(t.TempDir(), "audit-a")
	dirB := filepath.Join(t.TempDir(), "audit-b")
	rig := newSecLogRig(t, true, filepath.Join(dirA, "security.log"))

	stop := make(chan struct{})
	errCh := make(chan error, 8)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := rig.guardHitErr(); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var err error
			switch i % 3 {
			case 0:
				err = rig.reloadErr(true, filepath.Join(dirA, "security.log"))
			case 1:
				err = rig.reloadErr(false, "")
			case 2:
				err = rig.reloadErr(true, filepath.Join(dirB, "security.log"))
			}
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()

	// Run for a bounded number of full toggle cycles, then converge.
	waitUntil(t, "several audit reload cycles elapsed during the race", func() bool {
		return seclogRecordCount(t, dirA)+seclogRecordCount(t, dirB) >= 5
	})
	close(stop)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	rig.reload(t, true, filepath.Join(dirB, "security.log"))
	before := seclogRecordCount(t, dirB)
	rig.guardHit(t)
	waitUntil(t, "post-race audit record in the final directory", func() bool {
		return seclogRecordCount(t, dirB) > before
	})
}

// Close drains the current generation's logger: every accepted record is
// flushed before Close returns, and nothing is written afterwards.
func TestSecLogReload_CloseDrainsCurrentGeneration(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dir := filepath.Join(t.TempDir(), "audit")
	rig := newSecLogRig(t, true, filepath.Join(dir, "security.log"))

	const hits = 5
	for i := 0; i < hits; i++ {
		rig.guardHit(t)
	}
	logger := rig.proxy.SnapshotRuntime().SecLog
	if logger == nil {
		t.Fatal("snapshot must carry the audit logger")
	}
	rig.proxy.Close()

	// All hits were accepted before Close (enqueue is synchronous in forward),
	// so the drain must flush every one of them.
	if got := seclogRecordCount(t, dir); got != hits {
		t.Fatalf("records after Close = %d, want %d (Close must drain)", got, hits)
	}
	// A late enqueue against the drained logger is dropped, never written.
	auditGuardHit(logger, seclog.KindSecret, []string{"known_secret"}, "log", "late", "agent", "openai", "glm")
	if got := seclogRecordCount(t, dir); got != hits {
		t.Errorf("records after late enqueue = %d, want %d (no writes after Close)", got, hits)
	}
}
