//go:build unix

package server

import (
	"os"
	"syscall"
)

// noFollowFlag makes an open fail when the last component is a link; nonBlockFlag keeps the open
// of a FIFO or device from blocking (the descriptor is then rejected as not a regular file).
const (
	noFollowFlag = syscall.O_NOFOLLOW
	nonBlockFlag = syscall.O_NONBLOCK
)

// linkCount is the number of names of a file, when the platform reports it.
func linkCount(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

// clearNonblock puts a file opened with nonBlockFlag back into blocking mode.
func clearNonblock(f *os.File) error {
	return syscall.SetNonblock(int(f.Fd()), false)
}
