//go:build windows

package login

import (
	"fmt"
	"syscall"
	"time"
)

func (s *stdinEnterSource) Poll(timeout time.Duration) (bool, error) {
	millis := uint32(0)
	if timeout > 0 {
		millis = uint32((timeout + time.Millisecond - 1) / time.Millisecond)
	}
	event, err := syscall.WaitForSingleObject(syscall.Handle(s.file.Fd()), millis)
	if err != nil {
		return false, err
	}
	switch event {
	case syscall.WAIT_TIMEOUT:
		return false, nil
	case syscall.WAIT_OBJECT_0:
		return readLoginEnterByte(s.file)
	default:
		return false, fmt.Errorf("wait for stdin returned status %#x", event)
	}
}
