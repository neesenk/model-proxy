package serve

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// shutdown_test.go pins the process HTTP drain contract in-package (the
// subprocess-level CLI tests in internal/cli cannot attribute coverage here):
// handler admission gating, drain order, transport task join, and the SIGHUP
// reload loop.

// --- RunReloadLoop ---

func TestRunReloadLoopStopsWhenStopClosed(t *testing.T) {
	stop := make(chan struct{})
	hup := make(chan os.Signal, 1)
	close(stop)
	done := make(chan struct{})
	go func() { RunReloadLoop(stop, hup, func() { t.Error("reload must not run") }); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReloadLoop did not return after stop closed")
	}
}

func TestRunReloadLoopReloadsOnHup(t *testing.T) {
	stop := make(chan struct{})
	hup := make(chan os.Signal, 1)
	reloaded := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		RunReloadLoop(stop, hup, func() { reloaded <- struct{}{} })
		close(done)
	}()
	hup <- syscall.SIGHUP
	select {
	case <-reloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("reload was not invoked on SIGHUP")
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReloadLoop did not return after stop closed")
	}
}

func TestRunReloadLoopStopHasPriorityOverBufferedHup(t *testing.T) {
	stop := make(chan struct{})
	hup := make(chan os.Signal, 1)
	hup <- syscall.SIGHUP // buffered before stop closes
	close(stop)
	RunReloadLoop(stop, hup, func() { t.Error("reload must lose to a closed stop") })
}

// --- TransportHandlerGate ---

func TestTransportHandlerGateServesWhileAccepting(t *testing.T) {
	gate := NewTransportHandlerGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	gate.StopAccepting()
	gate.wait()
}

func TestTransportHandlerGateRejectsAfterStopAccepting(t *testing.T) {
	gate := NewTransportHandlerGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next handler must not run after StopAccepting")
	}))
	gate.StopAccepting()
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestTransportHandlerGateWaitBlocksForInflightHandler(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	gate := NewTransportHandlerGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))

	reqDone := make(chan struct{})
	go func() {
		gate.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(reqDone)
	}()
	<-entered // handler is admitted and blocked

	gate.StopAccepting()
	waitDone := make(chan struct{})
	go func() { gate.wait(); close(waitDone) }()
	select {
	case <-waitDone:
		t.Fatal("wait returned while a handler is still in flight")
	default:
	}
	close(release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted handler did not return after release")
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after the in-flight handler finished")
	}
}

// --- ServeHTTPUntilShutdown ---

// shutdownFixture starts a real listener on 127.0.0.1:0 and returns the pieces
// ServeHTTPUntilShutdown needs.
func shutdownFixture(t *testing.T, handler http.Handler) (*http.Server, net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: handler}
	return server, listener, "http://" + listener.Addr().String()
}

func TestServeHTTPUntilShutdownDrainsInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	server, listener, base := shutdownFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	taskStopped := make(chan struct{})
	task := func(stop <-chan struct{}) { <-stop; record("task"); close(taskStopped) }
	shutdown := make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- ServeHTTPUntilShutdown(server, listener, shutdown, 2*time.Second,
			[]TransportTask{task, nil}, func() { record("closeProxy") })
	}()

	// A live request must succeed before shutdown.
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET before shutdown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	close(shutdown)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTPUntilShutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTPUntilShutdown did not return")
	}

	select {
	case <-taskStopped:
	default:
		t.Fatal("transport task was not stopped/joined")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"task", "closeProxy"}
	if len(order) != len(want) {
		t.Fatalf("teardown order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("teardown order = %v, want %v", order, want)
		}
	}

	// After return the server is closed: new requests must fail.
	if _, err := http.Get(base + "/"); err == nil {
		t.Fatal("server still accepting after shutdown returned")
	}
}

func TestServeHTTPUntilShutdownReturnsServeError(t *testing.T) {
	server, listener, _ := shutdownFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	shutdown := make(chan struct{})
	proxyClosed := make(chan struct{})
	err := ServeHTTPUntilShutdown(server, listener, shutdown, time.Second, nil, func() { close(proxyClosed) })
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("err = %v, want the listener's serve error", err)
	}
	select {
	case <-proxyClosed:
	default:
		t.Fatal("closeProxy was not called on the serve-error path")
	}
}

func TestServeHTTPUntilShutdownForceClosesStuckHandler(t *testing.T) {
	// Handler parks on the request context: the drain deadline must expire and
	// the force-close must cancel it so teardown can complete.
	entered := make(chan struct{})
	handlerReturned := make(chan struct{})
	server, listener, _ := shutdownFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(handlerReturned)
	}))

	shutdown := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTPUntilShutdown(server, listener, shutdown, 50*time.Millisecond, nil, nil)
	}()

	// Open a request whose response never arrives on its own.
	reqDone := make(chan struct{})
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String() + "/")
		if err == nil {
			resp.Body.Close()
		}
		close(reqDone)
	}()

	// Wait until the stuck handler is actually admitted, then ask for shutdown
	// with a drain budget shorter than the handler's lifetime.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stuck handler was never admitted")
	}
	close(shutdown)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTPUntilShutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("force-close path did not complete")
	}
	select {
	case <-handlerReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("stuck handler was not cancelled by the force-close")
	}
	<-reqDone
}
