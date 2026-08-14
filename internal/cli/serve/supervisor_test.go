package serve

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
)

type fakeSupervisorWorker struct {
	pid      int
	waitCh   chan error
	signals  chan os.Signal
	killed   chan struct{}
	graceful bool
	waitOnce sync.Once
	killOnce sync.Once
}

func newFakeSupervisorWorker(pid int, graceful bool) *fakeSupervisorWorker {
	return &fakeSupervisorWorker{
		pid:      pid,
		waitCh:   make(chan error, 1),
		signals:  make(chan os.Signal, 8),
		killed:   make(chan struct{}),
		graceful: graceful,
	}
}

func (w *fakeSupervisorWorker) PID() int    { return w.pid }
func (w *fakeSupervisorWorker) Wait() error { return <-w.waitCh }

func (w *fakeSupervisorWorker) Signal(sig os.Signal) error {
	w.signals <- sig
	if w.graceful && sig == syscall.SIGTERM {
		w.waitOnce.Do(func() { w.waitCh <- nil })
	}
	return nil
}

func (w *fakeSupervisorWorker) Kill() error {
	w.killOnce.Do(func() {
		close(w.killed)
		w.waitOnce.Do(func() { w.waitCh <- errors.New("killed") })
	})
	return nil
}

func supervisorTestEnv(t *testing.T) (DaemonEnv, Args, string) {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "daemon", "model-proxy.log")
	if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
		t.Fatalf("create log directory: %v", err)
	}
	sa := Args{Config: "test-config.yaml"}
	env := DaemonEnv{LoadConfig: func(path string) (*configdomain.Config, error) {
		if path != sa.Config {
			t.Fatalf("LoadConfig path = %q, want %q", path, sa.Config)
		}
		return &configdomain.Config{LogFile: logFile}, nil
	}}
	return env, sa, PidFilePath(logFile)
}

func readyTimer() <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

func neverTimer() <-chan time.Time { return make(chan time.Time) }

func waitTestSignal(t *testing.T, ch <-chan os.Signal) os.Signal {
	t.Helper()
	select {
	case sig := <-ch:
		return sig
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for worker signal")
		return nil
	}
}

func waitSupervisorResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runSupervisor: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runSupervisor did not return")
	}
}

func TestRunSupervisorSpawnFailureBackoff(t *testing.T) {
	env, sa, _ := supervisorTestEnv(t)
	sigCh := make(chan os.Signal, 1)
	worker := newFakeSupervisorWorker(7001, true)
	var spawnCalls int
	var backoffs []time.Duration

	deps := supervisorDeps{
		spawn: func(DaemonEnv, Args) supervisorWorker {
			spawnCalls++
			if spawnCalls <= 2 {
				return nil
			}
			sigCh <- syscall.SIGTERM
			return worker
		},
		signals: sigCh,
		after: func(d time.Duration) <-chan time.Time {
			if d == SupervisorWorkerStopWait {
				return neverTimer()
			}
			backoffs = append(backoffs, d)
			return readyTimer()
		},
		now: time.Now,
		pid: func() int { return 7000 },
	}

	if err := runSupervisor(env, sa, deps); err != nil {
		t.Fatalf("runSupervisor: %v", err)
	}
	if spawnCalls != 3 {
		t.Fatalf("spawn calls = %d, want 3", spawnCalls)
	}
	if len(backoffs) != 2 || backoffs[0] != time.Second || backoffs[1] != 2*time.Second {
		t.Fatalf("spawn-failure backoffs = %v, want [1s 2s]", backoffs)
	}
	if got := waitTestSignal(t, worker.signals); got != syscall.SIGTERM {
		t.Fatalf("worker signal = %v, want SIGTERM", got)
	}
}

func TestRunSupervisorRestartsCrashedWorkerAfterBackoff(t *testing.T) {
	env, sa, _ := supervisorTestEnv(t)
	sigCh := make(chan os.Signal, 1)
	crashed := newFakeSupervisorWorker(7051, false)
	crashed.waitCh <- errors.New("worker crashed")
	restarted := newFakeSupervisorWorker(7052, true)
	firstSpawned := make(chan struct{}, 1)
	secondSpawned := make(chan struct{}, 1)
	backoffRequested := make(chan time.Duration, 1)
	backoffElapsed := make(chan time.Time, 1)
	var spawnCalls atomic.Int32

	deps := supervisorDeps{
		spawn: func(DaemonEnv, Args) supervisorWorker {
			switch spawnCalls.Add(1) {
			case 1:
				firstSpawned <- struct{}{}
				return crashed
			case 2:
				secondSpawned <- struct{}{}
				return restarted
			default:
				t.Fatal("unexpected third worker spawn")
				return nil
			}
		},
		signals: sigCh,
		after: func(d time.Duration) <-chan time.Time {
			if d == time.Second {
				backoffRequested <- d
				return backoffElapsed
			}
			return neverTimer()
		},
		now: func() time.Time { return time.Unix(100, 0) },
		pid: func() int { return 7050 },
	}
	done := make(chan error, 1)
	go func() { done <- runSupervisor(env, sa, deps) }()
	<-firstSpawned

	select {
	case got := <-backoffRequested:
		if got != time.Second {
			t.Fatalf("restart backoff = %s, want 1s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not enter restart backoff")
	}
	if got := spawnCalls.Load(); got != 1 {
		t.Fatalf("spawn calls before backoff elapsed = %d, want 1", got)
	}
	backoffElapsed <- time.Time{}
	select {
	case <-secondSpawned:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not spawn replacement worker")
	}

	sigCh <- syscall.SIGTERM
	if got := waitTestSignal(t, restarted.signals); got != syscall.SIGTERM {
		t.Fatalf("replacement worker signal = %v, want SIGTERM", got)
	}
	waitSupervisorResult(t, done)
	if got := spawnCalls.Load(); got != 2 {
		t.Fatalf("spawn calls = %d, want 2", got)
	}
	select {
	case sig := <-crashed.signals:
		t.Fatalf("crashed worker received unexpected signal %v", sig)
	default:
	}
}

func TestRunSupervisorForwardsSIGHUPWithoutRespawn(t *testing.T) {
	env, sa, _ := supervisorTestEnv(t)
	sigCh := make(chan os.Signal, 4)
	worker := newFakeSupervisorWorker(7101, true)
	spawned := make(chan struct{}, 1)
	secondSpawn := make(chan struct{}, 1)
	var spawnCalls atomic.Int32

	deps := supervisorDeps{
		spawn: func(DaemonEnv, Args) supervisorWorker {
			if spawnCalls.Add(1) == 1 {
				spawned <- struct{}{}
				return worker
			}
			secondSpawn <- struct{}{}
			return newFakeSupervisorWorker(7102, true)
		},
		signals: sigCh,
		after:   func(time.Duration) <-chan time.Time { return neverTimer() },
		now:     time.Now,
		pid:     func() int { return 7100 },
	}
	done := make(chan error, 1)
	go func() { done <- runSupervisor(env, sa, deps) }()
	<-spawned

	for i := 0; i < 2; i++ {
		sigCh <- syscall.SIGHUP
		select {
		case sig := <-worker.signals:
			if sig != syscall.SIGHUP {
				t.Fatalf("forwarded signal %d = %v, want SIGHUP", i, sig)
			}
		case <-secondSpawn:
			t.Fatal("SIGHUP spawned a second worker while the first was still running")
		case err := <-done:
			t.Fatalf("supervisor returned after SIGHUP: %v", err)
		case <-time.After(time.Second):
			t.Fatal("SIGHUP was not forwarded")
		}
	}

	sigCh <- syscall.SIGTERM
	if got := waitTestSignal(t, worker.signals); got != syscall.SIGTERM {
		t.Fatalf("shutdown signal = %v, want SIGTERM", got)
	}
	waitSupervisorResult(t, done)
	if got := spawnCalls.Load(); got != 1 {
		t.Fatalf("spawn calls = %d, want 1", got)
	}
}

func TestRunSupervisorSIGTERMWaitsAndCleansPID(t *testing.T) {
	env, sa, pidPath := supervisorTestEnv(t)
	sigCh := make(chan os.Signal, 1)
	worker := newFakeSupervisorWorker(7201, true)
	spawned := make(chan struct{}, 1)
	deps := supervisorDeps{
		spawn: func(DaemonEnv, Args) supervisorWorker {
			spawned <- struct{}{}
			return worker
		},
		signals: sigCh,
		after:   func(time.Duration) <-chan time.Time { return neverTimer() },
		now:     time.Now,
		pid:     func() int { return 7200 },
	}
	done := make(chan error, 1)
	go func() { done <- runSupervisor(env, sa, deps) }()
	<-spawned

	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read live pid file: %v", err)
	}
	if got, want := string(pidBytes), "7200\n"; got != want {
		t.Fatalf("pid file = %q, want %q", got, want)
	}

	sigCh <- syscall.SIGTERM
	if got := waitTestSignal(t, worker.signals); got != syscall.SIGTERM {
		t.Fatalf("worker signal = %v, want SIGTERM", got)
	}
	waitSupervisorResult(t, done)
	select {
	case <-worker.killed:
		t.Fatal("graceful worker was killed")
	default:
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("pid file remains after supervisor exit: %v", err)
	}
}

func TestRunSupervisorSIGTERMKillsAfterTimeout(t *testing.T) {
	env, sa, _ := supervisorTestEnv(t)
	sigCh := make(chan os.Signal, 1)
	worker := newFakeSupervisorWorker(7301, false)
	spawned := make(chan struct{}, 1)
	deps := supervisorDeps{
		spawn: func(DaemonEnv, Args) supervisorWorker {
			spawned <- struct{}{}
			return worker
		},
		signals: sigCh,
		after: func(d time.Duration) <-chan time.Time {
			if d != SupervisorWorkerStopWait {
				t.Fatalf("timer duration = %s, want %s", d, SupervisorWorkerStopWait)
			}
			return readyTimer()
		},
		now: time.Now,
		pid: func() int { return 7300 },
	}
	done := make(chan error, 1)
	go func() { done <- runSupervisor(env, sa, deps) }()
	<-spawned

	sigCh <- syscall.SIGTERM
	if got := waitTestSignal(t, worker.signals); got != syscall.SIGTERM {
		t.Fatalf("worker signal = %v, want SIGTERM", got)
	}
	waitSupervisorResult(t, done)
	select {
	case <-worker.killed:
	default:
		t.Fatal("stuck worker was not killed after timeout")
	}
}
