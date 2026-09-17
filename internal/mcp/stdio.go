package mcp

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

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
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
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
	} else if c.readErr == nil {
		c.readErr = io.EOF
	}
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
func (c *StdioConn) Call(body []byte) ([]byte, error) {
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
			return nil, fmt.Errorf("stdio: child exited (stderr tail: %s)", tail(c.stderr.String(), 200))
		}
		return payload, nil
	case <-c.readDone:
		return nil, fmt.Errorf("stdio: child exited (stderr tail: %s)", tail(c.stderr.String(), 200))
	}
}

// Close kills and reaps the child. Idempotent.
func (c *StdioConn) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.stdin.Close()
	if c.cmd.Process != nil {
		c.cmd.Process.Kill()
	}
	c.cmd.Wait()
}

// tail returns the last n bytes of s for compact error messages.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
