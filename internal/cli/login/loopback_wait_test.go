package login

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

type fakeLoginEnterSource struct {
	polls   int
	pressed bool
	err     error
}

type notifyingEOFEnterSource struct {
	polled   chan struct{}
	returned chan struct{}
	once     bool
}

func (s *notifyingEOFEnterSource) Poll(time.Duration) (bool, error) {
	if !s.once {
		s.once = true
		close(s.polled)
		defer close(s.returned)
	}
	return false, io.EOF
}

func (s *fakeLoginEnterSource) Poll(time.Duration) (bool, error) {
	s.polls++
	return s.pressed, s.err
}

func TestWaitForLoginSignalCoreCallback(t *testing.T) {
	ls := &LoopbackServer{CookieCh: make(chan string, 1), ErrCh: make(chan error, 1)}
	ls.CookieCh <- "callback-signal"
	enter := &fakeLoginEnterSource{err: errors.New("stdin must not be polled")}

	if err := waitForLoginSignal(context.Background(), ls, enter); err != nil {
		t.Fatalf("waitForLoginSignal: %v", err)
	}
	if enter.polls != 0 {
		t.Fatalf("stdin polls = %d, want 0 after ready callback", enter.polls)
	}
}

func TestWaitForLoginSignalCoreEnter(t *testing.T) {
	ls := &LoopbackServer{CookieCh: make(chan string, 1), ErrCh: make(chan error, 1)}
	enter := &fakeLoginEnterSource{pressed: true}

	if err := waitForLoginSignal(context.Background(), ls, enter); err != nil {
		t.Fatalf("waitForLoginSignal: %v", err)
	}
	if enter.polls != 1 {
		t.Fatalf("stdin polls = %d, want 1", enter.polls)
	}
}

func TestWaitForLoginSignalCoreCallbackError(t *testing.T) {
	callbackErr := errors.New("callback listener failed")
	ls := &LoopbackServer{CookieCh: make(chan string, 1), ErrCh: make(chan error, 1)}
	ls.ErrCh <- callbackErr
	enter := &fakeLoginEnterSource{err: errors.New("stdin must not be polled")}

	err := waitForLoginSignal(context.Background(), ls, enter)
	if !errors.Is(err, callbackErr) {
		t.Fatalf("waitForLoginSignal error = %v, want %v", err, callbackErr)
	}
	if enter.polls != 0 {
		t.Fatalf("stdin polls = %d, want 0 after callback error", enter.polls)
	}
}

func TestWaitForLoginSignalCoreStdinEOFFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish func(context.CancelFunc, *LoopbackServer)
		want   error
	}{
		{
			name: "callback",
			finish: func(_ context.CancelFunc, ls *LoopbackServer) {
				ls.CookieCh <- "callback-signal"
			},
		},
		{
			name: "cancel",
			finish: func(cancel context.CancelFunc, _ *LoopbackServer) {
				cancel()
			},
			want: context.Canceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls := &LoopbackServer{CookieCh: make(chan string, 1), ErrCh: make(chan error, 1)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			enter := &notifyingEOFEnterSource{polled: make(chan struct{}), returned: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- waitForLoginSignal(ctx, ls, enter) }()

			select {
			case <-enter.polled:
			case <-time.After(time.Second):
				t.Fatal("stdin source was not polled")
			}
			<-enter.returned
			tc.finish(cancel, ls)
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Fatalf("waitForLoginSignal error = %v, want %v", err, tc.want)
				}
			case <-time.After(time.Second):
				t.Fatal("waitForLoginSignal remained blocked after stdin EOF fallback")
			}
		})
	}
}

func TestWaitForLoginSignalCoreTimeout(t *testing.T) {
	ls := &LoopbackServer{CookieCh: make(chan string, 1), ErrCh: make(chan error, 1)}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	enter := &fakeLoginEnterSource{err: errors.New("stdin must not be polled")}

	err := waitForLoginSignal(ctx, ls, enter)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForLoginSignal error = %v, want DeadlineExceeded", err)
	}
	if enter.polls != 0 {
		t.Fatalf("stdin polls = %d, want 0 after expired deadline", enter.polls)
	}
}

func TestWaitForLoginSignalCoreCancel(t *testing.T) {
	ls := &LoopbackServer{CookieCh: make(chan string, 1), ErrCh: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	enter := &fakeLoginEnterSource{err: errors.New("stdin must not be polled")}

	err := waitForLoginSignal(ctx, ls, enter)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForLoginSignal error = %v, want Canceled", err)
	}
	if enter.polls != 0 {
		t.Fatalf("stdin polls = %d, want 0 after cancellation", enter.polls)
	}
}

func TestStdinEnterSourcePoll(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatalf("write ENTER: %v", err)
	}

	pressed, err := (&stdinEnterSource{file: r}).Poll(time.Second)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !pressed {
		t.Fatal("Poll reported no ENTER for a ready newline")
	}
}

func TestStdinEnterSourcePollNoDataIsBounded(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})

	const pollTimeout = 20 * time.Millisecond
	started := time.Now()
	pressed, err := (&stdinEnterSource{file: r}).Poll(pollTimeout)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if pressed {
		t.Fatal("Poll reported ENTER with no pipe data")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Poll took %s for %s timeout", elapsed, pollTimeout)
	}
}
