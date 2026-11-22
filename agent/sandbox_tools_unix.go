//go:build !windows

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// isolateProcess puts the helper in a process group of its own and makes a timeout or a
// cancelled call kill the whole group (the helper may have started ripgrep).
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
}

// ownerUID is the owner of a file, when the platform reports one.
func ownerUID(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
