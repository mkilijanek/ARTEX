//go:build !linux && !windows

package sandbox

import (
	"syscall"
	"time"
)

// statChangeTime: other Unix systems name the field differently; without it the walk never
// treats a directory as unchanged.
func statChangeTime(*syscall.Stat_t) (time.Time, bool) { return time.Time{}, false }
