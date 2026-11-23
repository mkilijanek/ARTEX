//go:build !windows

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// oNoFollow makes an open fail when the final component is a symbolic link.
const oNoFollow = syscall.O_NOFOLLOW

// oNonblock keeps an open of a file that was swapped for a FIFO from blocking.
const oNonblock = syscall.O_NONBLOCK

// fileUID is the owner of a file, when the platform reports one.
func fileUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// isNoProcess reports ESRCH: the process is gone.
func isNoProcess(err error) bool { return err == syscall.ESRCH }

// killedBySIGKILL reports whether err is the exit of a process that was killed by SIGKILL.
func killedBySIGKILL(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// fileMeta is what the ownership walk reads from a Lstat result.
type fileMeta struct {
	UID, GID uint32
	Nlink    uint64
	// Changed is the later of the modification and the status-change time, when the platform
	// reports the latter; ok is false when it does not, and the caller then treats the entry
	// as changed.
	Changed   time.Time
	ChangedOK bool
}

// metaOf returns the owner and link count of fi. ok is false when the platform reports none.
func metaOf(fi os.FileInfo) (m fileMeta, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return m, false
	}
	m.UID, m.GID, m.Nlink = st.Uid, st.Gid, uint64(st.Nlink)
	m.Changed, m.ChangedOK = fi.ModTime(), false
	if ct, has := statChangeTime(st); has {
		m.ChangedOK = true
		if ct.After(m.Changed) {
			m.Changed = ct
		}
	}
	return m, true
}
