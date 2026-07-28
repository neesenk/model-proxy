package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"model-proxy/internal/protocol"
)

func TestServeHTTPUntilShutdownDrainsHandlerBeforeProxyFinalFlush(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "requests")
	statePath := filepath.Join(t.TempDir(), "responses_state.json")
	p := &Proxy{
		lifecycle:      newProxyLifecycle(),
		responsesState: protocol.NewResponsesStateStore(statePath),
	}
	t.Cleanup(p.Close)
	p.reqLog = newRequestLogger(logDir, 1<<20, 1<<10, 0)
	p.reqLogStarted = p.lifecycle.run(func(<-chan struct{}) {
		p.reqLog.loop()
	})
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}

	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerErr := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(handlerStarted)
		<-releaseHandler

		// These writes happen at the end of an in-flight handler. Closing Proxy
		// before HTTP drain would either lose them or write into stopped owners.
		p.reqLog.record(&requestLogRecord{RequestID: "req-inflight"})
		history := []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		}}
		response := []byte(`{"id":"resp-inflight","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
		if !p.responsesState.RecordJSON("sess", history, response) {
			handlerErr <- "responses state rejected in-flight record"
		}
		_, _ = io.WriteString(w, "ok")
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	shutdownStarted := make(chan struct{})
	server.RegisterOnShutdown(func() {
		close(shutdownStarted)
	})

	shutdown := make(chan struct{})
	gcStopped := make(chan struct{})
	orderingErr := make(chan string, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveHTTPUntilShutdown(
			server,
			listener,
			shutdown,
			time.Second,
			[]transportTask{func(stop <-chan struct{}) {
				webGC(stop, newLoginSessionStore())
				close(gcStopped)
			}},
			func() {
				select {
				case <-gcStopped:
				default:
					orderingErr <- "Proxy.Close ran before Web GC stopped"
				}
				p.Close()
			},
		)
	}()

	clientDone := make(chan error, 1)
	go func() {
		resp, requestErr := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + listener.Addr().String())
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			requestErr = resp.Body.Close()
		}
		clientDone <- requestErr
	}()
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	close(shutdown)
	select {
	case <-shutdownStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP shutdown did not begin")
	}
	select {
	case <-gcStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Web GC did not stop with transport shutdown")
	}

	// Shutdown is now waiting on the blocked handler. The logger must remain
	// open; Responses-state ordering is asserted below by requiring the
	// in-flight record to survive the final close-time persistence.
	select {
	case <-p.reqLog.closed:
		t.Fatal("request logger closed before the in-flight handler completed")
	default:
	}
	select {
	case err := <-serveDone:
		t.Fatalf("serve returned before the in-flight handler completed: %v", err)
	default:
	}

	close(releaseHandler)
	select {
	case err := <-clientDone:
		if err != nil {
			t.Fatalf("client request failed during graceful drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client request did not complete after handler release")
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not finish after handler drain")
	}
	select {
	case msg := <-handlerErr:
		t.Fatal(msg)
	default:
	}
	select {
	case msg := <-orderingErr:
		t.Fatal(msg)
	default:
	}
	select {
	case <-p.reqLog.closed:
	default:
		t.Fatal("request logger was not closed after handler drain")
	}
	assertFileTreeContains(t, logDir, "req-inflight")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read final Responses state: %v", err)
	}
	if !strings.Contains(string(stateData), "resp-inflight") {
		t.Fatalf("final Responses state omitted in-flight record: %s", stateData)
	}
}

func TestServeHTTPUntilShutdownForceClosesConnectionsAfterDeadline(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerCancelled := make(chan struct{})
	releaseCancelledHandler := make(chan struct{})
	handlerReturned := make(chan struct{})
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-r.Context().Done()
		close(handlerCancelled)
		<-releaseCancelledHandler
		close(handlerReturned)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	shutdown := make(chan struct{})
	proxyClosed := make(chan struct{})
	orderingErr := make(chan string, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveHTTPUntilShutdown(
			server,
			listener,
			shutdown,
			20*time.Millisecond,
			nil,
			func() {
				select {
				case <-handlerCancelled:
				default:
					orderingErr <- "Proxy.Close ran before force-close cancelled the handler"
				}
				close(proxyClosed)
			},
		)
	}()

	clientDone := make(chan error, 1)
	go func() {
		resp, requestErr := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + listener.Addr().String())
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		clientDone <- requestErr
	}()
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	close(shutdown)
	select {
	case <-handlerCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("force-close did not cancel the handler context")
	}
	select {
	case <-proxyClosed:
		t.Fatal("Proxy closed while the cancelled handler was still unwinding")
	default:
	}
	select {
	case err := <-serveDone:
		t.Fatalf("serve returned before the cancelled handler unwound: %v", err)
	default:
	}

	close(releaseCancelledHandler)
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve returned error after forced close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not finish after cancelled handler returned")
	}
	select {
	case <-handlerReturned:
	default:
		t.Fatal("cancelled handler did not return before serve completed")
	}
	select {
	case <-proxyClosed:
	default:
		t.Fatal("Proxy close callback did not run")
	}
	select {
	case msg := <-orderingErr:
		t.Fatal(msg)
	default:
	}
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not observe forced connection close")
	}
}

func TestServeHTTPUntilShutdownCleansUpAfterServeError(t *testing.T) {
	baseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptFailure := errors.New("injected accept failure")
	failAccept := make(chan struct{})
	listener := &failAfterFirstListener{
		Listener: baseListener,
		fail:     failAccept,
		err:      acceptFailure,
	}

	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-r.Context().Done()
		close(handlerDone)
	})}

	taskStarted := make(chan struct{})
	taskStopped := make(chan struct{})
	var proxyCloseCalls atomic.Int32
	orderingErr := make(chan string, 1)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveHTTPUntilShutdown(
			server,
			listener,
			make(chan struct{}),
			time.Second,
			[]transportTask{func(stop <-chan struct{}) {
				close(taskStarted)
				<-stop
				close(taskStopped)
			}},
			func() {
				select {
				case <-handlerDone:
				default:
					orderingErr <- "Proxy.Close ran before the admitted handler returned"
				}
				select {
				case <-taskStopped:
				default:
					orderingErr <- "Proxy.Close ran before the transport task stopped"
				}
				proxyCloseCalls.Add(1)
			},
		)
	}()

	select {
	case <-taskStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("transport task did not start")
	}
	clientDone := make(chan error, 1)
	go func() {
		resp, requestErr := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + baseListener.Addr().String())
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			requestErr = resp.Body.Close()
		}
		clientDone <- requestErr
	}()
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}

	// The next Accept fails while one admitted handler is still active. The
	// error path must cancel and join that handler and every transport task
	// before it runs the Proxy close callback.
	close(failAccept)
	select {
	case serveErr := <-serveDone:
		if serveErr == nil || !strings.Contains(serveErr.Error(), acceptFailure.Error()) {
			t.Fatalf("serve error = %v, want injected accept failure", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve error path did not return")
	}
	select {
	case <-handlerDone:
	default:
		t.Fatal("serve error path did not wait for the admitted handler")
	}
	select {
	case <-taskStopped:
	default:
		t.Fatal("serve error path did not stop the transport task")
	}
	if got := proxyCloseCalls.Load(); got != 1 {
		t.Fatalf("Proxy close callback calls = %d, want 1", got)
	}
	select {
	case msg := <-orderingErr:
		t.Fatal(msg)
	default:
	}
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not observe the forced close")
	}
}

func TestRunReloadLoopStopsBufferedSignalsAndJoinsActiveReload(t *testing.T) {
	stop := make(chan struct{})
	hup := make(chan os.Signal, 1)
	reloadStarted := make(chan struct{})
	releaseReload := make(chan struct{})
	loopDone := make(chan struct{})
	var calls atomic.Int32
	go func() {
		runReloadLoop(stop, hup, func() {
			calls.Add(1)
			close(reloadStarted)
			<-releaseReload
		})
		close(loopDone)
	}()

	hup <- syscall.SIGHUP
	select {
	case <-reloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reload did not start")
	}
	close(stop)
	hup <- syscall.SIGHUP // buffered while the active reload is finishing
	select {
	case <-loopDone:
		t.Fatal("reload loop returned before the active reload completed")
	default:
	}
	close(releaseReload)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reload loop did not return after the active reload completed")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("reload calls = %d, want only the already-active reload", got)
	}

	// A stop that wins before the loop starts must also suppress an already
	// buffered SIGHUP.
	stopped := make(chan struct{})
	close(stopped)
	buffered := make(chan os.Signal, 1)
	buffered <- syscall.SIGHUP
	runReloadLoop(stopped, buffered, func() {
		t.Fatal("buffered SIGHUP started a reload after shutdown")
	})
}

func TestGracefulShutdownFitsSupervisorHardStopWindow(t *testing.T) {
	if gracefulShutdownTimeout >= supervisorWorkerStopWait {
		t.Fatalf(
			"graceful shutdown timeout %s must stay below supervisor hard-stop wait %s",
			gracefulShutdownTimeout,
			supervisorWorkerStopWait,
		)
	}
}

type failAfterFirstListener struct {
	net.Listener
	fail  <-chan struct{}
	err   error
	first bool
}

func (l *failAfterFirstListener) Accept() (net.Conn, error) {
	if !l.first {
		l.first = true
		return l.Listener.Accept()
	}
	<-l.fail
	return nil, l.err
}

func assertFileTreeContains(t *testing.T, root, want string) {
	t.Helper()
	found := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), want) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	if !found {
		t.Fatalf("%q not found under %s", want, root)
	}
}
