package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// maxStderrTail bounds how much of a dead child's stderr the failure
// diagnostics carry: the end of the output is where a fatal error lives.
const maxStderrTail = 512

// minSecretLen is the shortest environment value worth masking. Below it a
// value is a flag or a mode ("1", "true"), and masking it would shred
// ordinary words in the child's output.
const minSecretLen = 8

// benignEnv names the environment variables a parent hands down that are not
// credentials and routinely appear in diagnostics (search paths, the home
// directory, the locale). Every OTHER value the child was given is treated as
// a secret: stdio servers receive their credentials through env, and a child
// dying on a bad key tends to print what it was handed.
var benignEnv = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TZ": true}

// ExitError is a failure caused by the child process dying. Err is the bare
// failure — all an HTTP answer may carry, because child stderr is untrusted
// and may hold credentials. Diag adds what an operator needs to tell why the
// child died: its wait status and a bounded, single-line tail of its stderr
// with the credential-bearing environment values redacted.
type ExitError struct {
	Err  error
	Diag string
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitDiag returns the redacted diagnostics a stdio failure carries, or ""
// when the failure is not a dead child. For operator surfaces only (serve.log,
// the CLI probe) — never for the HTTP answer.
func ExitDiag(err error) string {
	var e *ExitError
	if errors.As(err, &e) {
		return e.Diag
	}
	return ""
}

// redactTail flattens the END of a child's stderr into one bounded line with
// the credential-bearing environment values masked. Redaction runs over the
// whole capture BEFORE the cut, so the tail can never split a secret (or a
// placeholder) in two.
func redactTail(stderr string, env []string) string {
	type secret struct{ name, value string }
	var secrets []secret
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if benignEnv[name] || len(value) < minSecretLen {
			continue
		}
		secrets = append(secrets, secret{name, value})
	}
	// Longest first: when one secret contains another, the inner one must not
	// leave the outer one's remainder behind.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i].value) > len(secrets[j].value) })
	flat := stderr
	for _, s := range secrets {
		flat = strings.ReplaceAll(flat, s.value, "<redacted:"+s.name+">")
	}
	// One line: serve.log is read line by line, and a multi-line stack trace
	// would pour across it as unattributed lines.
	flat = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, flat)), " ")
	if len(flat) <= maxStderrTail {
		return flat
	}
	tail := flat[len(flat)-maxStderrTail:]
	for len(tail) > 0 && tail[0]&0xc0 == 0x80 { // the cut may have split a UTF-8 rune
		tail = tail[1:]
	}
	return "…" + tail
}

// StdioConn owns one MCP stdio child process: newline-delimited JSON-RPC 2.0
// messages on stdin/stdout (the MCP stdio transport), responses correlated to
// requests by their JSON-RPC id. The conn is the child's sole owner — Close
// kills and reaps it. A dead read loop fails every pending and future Call
// (no implicit restart: the session layer re-spawns on re-initialize).
type StdioConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *ringBuffer

	mu      sync.Mutex // serializes writes and guards pending
	pending map[string]chan []byte

	readDone chan struct{}
	readErr  error // first fatal read-loop error (set before readDone closes)

	waitOnce sync.Once // cmd.Wait runs exactly once (read-loop EOF path and Close)

	closed atomic.Bool
}

// ringBuffer is a bounded tail buffer for the child's stderr (diagnostics).
type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	cap  int
}

func newRingBuffer(capBytes int) *ringBuffer { return &ringBuffer{cap: capBytes} }

func (b *ringBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > b.cap {
		b.data = b.data[len(b.data)-b.cap:]
	}
	return len(p), nil
}

func (b *ringBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// MaxFrameBytes bounds one newline-delimited JSON-RPC frame the gateway reads
// from a stdio child (the read loop's scanner buffer cap). A longer line is
// not truncated: the read loop fails (bufio.ErrTooLong) and the whole conn
// dies with every pending call on it. Children must therefore keep every
// written line strictly below it — the newline terminator shares the buffer
// with the line, so the longest frame that gets through is MaxFrameBytes-1
// bytes of JSON plus the newline. Bundled children derive their output bound
// from this value.
const MaxFrameBytes = 16 * 1024 * 1024

// StartStdio launches the child with the given environment (already resolved
// by the caller — credential templating happens in the app layer) and starts
// the response-demultiplexing read loop.
func StartStdio(command []string, env []string) (*StdioConn, error) {
	if len(command) == 0 || command[0] == "" {
		return nil, fmt.Errorf("stdio: empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdio: stdout pipe: %w", err)
	}
	stderr := newRingBuffer(8 * 1024)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("stdio: start %q: %w", command[0], err)
	}
	c := &StdioConn{
		cmd:      cmd,
		stdin:    stdin,
		stderr:   stderr,
		pending:  map[string]chan []byte{},
		readDone: make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c, nil
}

// readLoop demultiplexes stdout lines into pending response channels.
// Notifications and responses with no pending caller are discarded.
func (c *StdioConn) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		frame := ParseFrame(line)
		if !frame.IsResponse || len(frame.ID) == 0 {
			continue // notification or opaque line (e.g. server logs on stdout)
		}
		key := string(frame.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		if ok {
			delete(c.pending, key)
		}
		c.mu.Unlock()
		if ok {
			payload := make([]byte, len(line))
			copy(payload, line)
			ch <- payload
		}
	}
	if err := scanner.Err(); err != nil {
		c.readErr = err
		// A read error is not the child's exit: on an oversized frame
		// (ErrTooLong) the child is typically still alive, blocked writing the
		// rest of the line, and the reap below waits for its exit. Kill it
		// first — otherwise every pending caller waits with it until its own
		// timeout tears the conn down from above.
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	} else if c.readErr == nil {
		c.readErr = io.EOF
	}
	// Reap before signalling callers: os/exec drains cmd.Stderr (an io.Writer
	// — its copy goroutine is the only writer) inside Wait, so only a completed
	// Wait guarantees Stderr() holds the child's complete output. Callers that
	// read Stderr() after a failed Call would otherwise race the copy goroutine
	// and see a truncated/empty tail (flaked under CI load in coverage mode).
	c.reapChild()
	// Fail every pending caller: the child is gone.
	c.mu.Lock()
	for key, ch := range c.pending {
		delete(c.pending, key)
		close(ch)
	}
	c.mu.Unlock()
	close(c.readDone)
}

// Call sends one JSON-RPC message and waits for its response. Notifications
// (no id) return nil immediately after the write. A closed/dead conn errors.
// ctx bounds the wait: cancellation aborts the pending entry and returns —
// a hung child can never park a caller forever.
func (c *StdioConn) Call(ctx context.Context, body []byte) ([]byte, error) {
	if c.closed.Load() {
		return nil, fmt.Errorf("stdio: conn closed")
	}
	frame := ParseFrame(body)
	if !frame.HasID || frame.IsResponse {
		// Notification semantics: write and done.
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, err := c.stdin.Write(append(body, '\n')); err != nil {
			return nil, fmt.Errorf("stdio: write: %w", err)
		}
		return nil, nil
	}
	key := string(frame.ID)
	ch := make(chan []byte, 1)
	c.mu.Lock()
	if _, dup := c.pending[key]; dup {
		c.mu.Unlock()
		return nil, fmt.Errorf("stdio: duplicate in-flight id %s", key)
	}
	c.pending[key] = ch
	_, err := c.stdin.Write(append(body, '\n'))
	c.mu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return nil, fmt.Errorf("stdio: write: %w", err)
	}
	select {
	case payload, ok := <-ch:
		if !ok {
			return nil, c.exitError()
		}
		return payload, nil
	case <-c.readDone:
		return nil, c.exitError()
	case <-ctx.Done():
		// Drop the pending entry so a late response is discarded (readLoop
		// tolerates missing entries) instead of leaking the registration.
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return nil, fmt.Errorf("stdio: call: %w", ctx.Err())
	}
}

// exitError reports a dead child. Valid only once readDone is closed or the
// pending channel was closed: the read loop reaps the child before closing
// either, so the wait status and the stderr capture are final by then.
func (c *StdioConn) exitError() error {
	status := "unknown status"
	if c.cmd.ProcessState != nil {
		status = c.cmd.ProcessState.String() // "exit status 1", "signal: killed"
	}
	diag := status
	if tail := redactTail(c.stderr.String(), c.cmd.Env); tail != "" {
		diag += "; stderr: " + tail
	}
	return &ExitError{Err: fmt.Errorf("stdio: child exited (%s)", status), Diag: diag}
}

// Stderr returns the captured tail of the child's stderr, unredacted: the raw
// capture, for callers that redact it themselves. Operator surfaces should
// read ExitDiag on the failure instead.
func (c *StdioConn) Stderr() string {
	return c.stderr.String()
}

// reapChild waits for the child exactly once (the read loop's EOF path and
// Close can both get here first). Wait also completes the internal stderr
// copy goroutine, so after it returns Stderr() is final.
func (c *StdioConn) reapChild() {
	c.waitOnce.Do(func() { _ = c.cmd.Wait() })
}

// Close kills and reaps the child. Idempotent.
func (c *StdioConn) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	c.reapChild()
}
