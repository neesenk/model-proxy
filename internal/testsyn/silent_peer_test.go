package testsyn

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// The silent-peer fixture's whole value is its buffering contract: nothing
// crosses until the peer reads. A pipe that quietly buffered would turn every
// "client write blocks" test built on it into a test of nothing.

func TestSilentPeerListenerWriteBlocksUntilPeerReads(t *testing.T) {
	listener := NewSilentPeerListener()
	t.Cleanup(func() { _ = listener.Close() })
	client, err := listener.Connect()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	serverEnd, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverEnd.Close() })

	// With the peer silent, a write sits until its own deadline fires — the
	// property the stalled-client tests rely on.
	_ = serverEnd.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := serverEnd.Write([]byte("data")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write with a silent peer = %v, want the write deadline; the pipe buffered it", err)
	}
	// A timed-out write leaves nothing behind: the peer's first read gets
	// exactly what a later write sends, once it reads.
	_ = serverEnd.SetWriteDeadline(time.Time{})
	written := make(chan error, 1)
	go func() {
		_, err := serverEnd.Write([]byte("data"))
		written <- err
	}()
	got := make([]byte, 4)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write with a reading peer = %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("peer read %q, want the one write that followed its read", got)
	}
}

func TestSilentPeerListenerCloseUnblocksAcceptAndFailsConnect(t *testing.T) {
	listener := NewSilentPeerListener()
	accepted := make(chan error, 1)
	go func() {
		_, err := listener.Accept()
		accepted <- err
	}()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-accepted:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close left a parked Accept behind")
	}
	if _, err := listener.Connect(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Connect after Close = %v, want net.ErrClosed", err)
	}
	if network, address := listener.Addr().Network(), listener.Addr().String(); network != "pipe" || address != "pipe" {
		t.Fatalf("Addr = %q %q, want pipe/pipe", network, address)
	}
}
