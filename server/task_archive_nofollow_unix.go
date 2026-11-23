//go:build unix

package server

import (
	"io/fs"
	"syscall"
)

// archiveOpenFlags are added to every read open during packaging: a final component
// that is a link is refused (O_NOFOLLOW), and a swapped-in FIFO or device cannot
// block the open (O_NONBLOCK; the descriptor is then rejected as non-regular).
const archiveOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// archiveLinkCount is the number of hard links of the file info describes; ok is false when the
// platform does not say.
func archiveLinkCount(info fs.FileInfo) (n uint64, ok bool) {
	if st, isStat := info.Sys().(*syscall.Stat_t); isStat {
		return uint64(st.Nlink), true
	}
	return 0, false
}
