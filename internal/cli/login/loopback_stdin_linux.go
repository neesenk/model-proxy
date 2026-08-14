//go:build linux

package login

import (
	"fmt"
	"syscall"
	"time"
)

func (s *stdinEnterSource) Poll(timeout time.Duration) (bool, error) {
	fd := int(s.file.Fd())
	const bitsPerWord = 64
	var readSet syscall.FdSet
	word := fd / bitsPerWord
	if fd < 0 || word >= len(readSet.Bits) {
		return false, fmt.Errorf("stdin fd %d is outside select range", fd)
	}
	readSet.Bits[word] |= int64(1) << uint(fd%bitsPerWord)
	if timeout < 0 {
		timeout = 0
	}
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	if _, err := syscall.Select(fd+1, &readSet, nil, nil, &tv); err != nil {
		if err == syscall.EINTR {
			return false, nil
		}
		return false, err
	}
	if readSet.Bits[word]&(int64(1)<<uint(fd%bitsPerWord)) == 0 {
		return false, nil
	}
	return readLoginEnterByte(s.file)
}
