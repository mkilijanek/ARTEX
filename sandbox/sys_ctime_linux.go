//go:build linux

package sandbox

import (
	"syscall"
	"time"
)

// statChangeTime is the status-change time of an entry.
func statChangeTime(st *syscall.Stat_t) (time.Time, bool) {
	return time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec)), true
}
