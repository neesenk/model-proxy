// shutdown.go owns the process HTTP drain contract: explicit listener,
// handler-admission gate, transport tasks, and the SIGHUP reload loop.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"model-proxy/internal/observe/logx"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// GracefulShutdownTimeout stays below the supervisor's 10-second worker kill
// window so a worker that has to force-close stuck clients still has time to
// final-flush Proxy-owned logs and state before the supervisor's hard stop.
const GracefulShutdownTimeout = 8 * time.Second

// TransportTask is process transport work whose lifetime is bounded by the
// HTTP server, rather than by Proxy. Tasks must return when stop is closed.
// It is an alias so composition layers (e.g. internal/app Runtime) can hold
// task slices without importing the process layer.
type TransportTask = func(stop <-chan struct{})

// RunReloadLoop owns process SIGHUP handling. The double stop check gives
// shutdown priority over an already-buffered SIGHUP, while an in-progress
// reload is allowed to finish and is then joined by the transport task owner.
func RunReloadLoop(stop <-chan struct{}, hup <-chan os.Signal, reload func()) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		select {
		case <-stop:
			return
		case <-hup:
			select {
			case <-stop:
				return
			default:
			}
			reload()
		}
	}
}

// TransportHandlerGate makes HTTP handler admission atomic with shutdown. The
// standard library's Server.Close cancels active connections but does not
// promise their handlers have returned, so Proxy-owned state must not close
// until this gate's wait completes.
type TransportHandlerGate struct {
	mu        sync.Mutex
	accepting bool
	inFlight  int // admitted handlers that have not returned yet
	wg        sync.WaitGroup
	next      http.Handler
}

func NewTransportHandlerGate(next http.Handler) *TransportHandlerGate {
	if next == nil {
		next = http.DefaultServeMux
	}
	return &TransportHandlerGate{accepting: true, next: next}
}

// shuttingDownBody is the gate's answer once the process is draining: a
// machine-readable code a client can tell from any other 503 — the Web page
// switches to its "restarting" state on it — plus the retry hint. A drain
// ends within seconds, so a client that retries right away gets through.
type shuttingDownBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func (g *TransportHandlerGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if !g.accepting {
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(shuttingDownBody{Error: "server is shutting down", Code: "shutting_down"})
		return
	}
	g.wg.Add(1)
	g.inFlight++
	g.mu.Unlock()
	defer g.done()
	g.next.ServeHTTP(w, r)
}

// done releases one admitted handler.
func (g *TransportHandlerGate) done() {
	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
	g.wg.Done()
}

// inFlightHandlers reports how many admitted handlers have not returned — the
// number a drain deadline is about to force-close.
func (g *TransportHandlerGate) inFlightHandlers() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight
}

func (g *TransportHandlerGate) StopAccepting() {
	g.mu.Lock()
	g.accepting = false
	g.mu.Unlock()
}

func (g *TransportHandlerGate) wait() {
	g.wg.Wait()
}

// ServeHTTPUntilShutdown runs one explicit HTTP server until it fails or a stop
// is requested. Shutdown order is deliberately centralized:
//
//  1. reject new transport-owned work (reload and internal/web tasks);
//  2. stop accepting HTTP connections and drain in-flight handlers;
//  3. on deadline, force-close connections so request contexts are cancelled
//     (the warn names how many handlers that cut);
//  4. wait transport tasks, then close/final-flush Proxy-owned state.
//
// The explicit listener and closeProxy callback keep this sequence testable
// without sending real process signals or invoking os.Exit.
func ServeHTTPUntilShutdown(
	server *http.Server,
	listener net.Listener,
	shutdown <-chan struct{},
	timeout time.Duration,
	tasks []TransportTask,
	closeProxy func(),
) error {
	handlers := NewTransportHandlerGate(server.Handler)
	server.Handler = handlers

	transportStop := make(chan struct{})
	var transportWG sync.WaitGroup
	for _, task := range tasks {
		if task == nil {
			continue
		}
		transportWG.Add(1)
		go func(run TransportTask) {
			defer transportWG.Done()
			run(transportStop)
		}(task)
	}

	var stopOnce sync.Once
	stopTransport := func() {
		stopOnce.Do(func() {
			close(transportStop)
		})
	}
	defer func() {
		stopTransport()
		handlers.StopAccepting()
		// Close is idempotent. It guarantees cancellation has been requested
		// before waiting for admitted handlers to finish unwinding.
		_ = server.Close()
		handlers.wait()
		transportWG.Wait()
		if closeProxy != nil {
			closeProxy()
		}
	}()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		// Deferred teardown cancels and waits any still-active handlers before
		// Proxy state is closed.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-shutdown:
		stopTransport()
		handlers.StopAccepting()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	shutdownErr := server.Shutdown(ctx)
	cancel()
	if shutdownErr != nil {
		// The count is the blast radius: a force-close cuts every in-flight
		// stream mid-generation, and downstream those streams only surface as
		// per-request "client disconnected" lines (the executor cannot see who
		// closed the connection). The 2026-11-22 restart cut 37 streams and the
		// number took a request-log scan to establish.
		logx.Warnf("[shutdown] HTTP drain exceeded %s (%v); forcing active connections closed (in-flight handlers: %d)",
			timeout, shutdownErr, handlers.inFlightHandlers())
		if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			logx.Warnf("[shutdown] force-close failed: %v", closeErr)
		}
	}

	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// persistTokensLoop has been removed; per-minute persistence is now owned by
// internal/observe/stats.Store + the root statsFlushLoop, started through
// Proxy lifecycle services.
