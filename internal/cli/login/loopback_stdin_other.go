//go:build !darwin && !linux && !windows

package login

import (
	"fmt"
	"time"
)

func (s *stdinEnterSource) Poll(time.Duration) (bool, error) {
	return false, fmt.Errorf("cancellable stdin polling is unsupported on this platform")
}
