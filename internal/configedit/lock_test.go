package configedit

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// TestWithConfigLockSerializesRMW: concurrent read-modify-write cycles under
// WithConfigLock must all land (lost-update regression — the atomic
// temp+rename write alone keeps the file whole but lets a later writer
// silently discard an earlier writer's change).
func TestWithConfigLockSerializesRMW(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}
	const writers = 32
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := WithConfigLock(path, func() error {
				raw, rerr := os.ReadFile(path)
				if rerr != nil {
					return rerr
				}
				n, _ := strconv.Atoi(string(raw))
				return os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o644)
			})
			if err != nil {
				t.Errorf("WithConfigLock: %v", err)
			}
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != strconv.Itoa(writers) {
		t.Fatalf("lost update: counter = %s, want %d", got, writers)
	}
}

// TestWithConfigLockSerializesWholeWriteBeforeRMW models the two app writer
// shapes that share this primitive: a raw whole-document replacement already
// holding the lock, followed by a structured load-modify-write. The RMW must
// observe the raw replacement and preserve both changes.
func TestWithConfigLockSerializesWholeWriteBeforeRMW(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wholeWritten := make(chan struct{})
	releaseWhole := make(chan struct{})
	wholeErr := make(chan error, 1)
	go func() {
		wholeErr <- WithConfigLock(path, func() error {
			if err := os.WriteFile(path, []byte("raw\n"), 0o644); err != nil {
				return err
			}
			close(wholeWritten)
			<-releaseWhole
			return nil
		})
	}()
	<-wholeWritten

	rmwStarted := make(chan struct{})
	rmwErr := make(chan error, 1)
	go func() {
		close(rmwStarted)
		rmwErr <- WithConfigLock(path, func() error {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(path, append(raw, []byte("edit\n")...), 0o644)
		})
	}()
	<-rmwStarted
	close(releaseWhole)
	if err := <-wholeErr; err != nil {
		t.Fatalf("whole write: %v", err)
	}
	if err := <-rmwErr; err != nil {
		t.Fatalf("RMW: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), "raw\nedit\n"; got != want {
		t.Fatalf("serialized writers produced %q, want %q", got, want)
	}
}
