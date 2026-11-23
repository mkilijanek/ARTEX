//go:build !windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/mkilijanek/artex/sandbox"
	"golang.org/x/sys/unix"
)

// stageLayout names the paths the stages use. Tests point it at a temporary tree.
var stageLayout = sandbox.DefaultLayout()

// runAstool is the root stage of the drop chain: `artex __astool [--cd DIR] [--vars FILE] --
// PROGRAM ARGS...`. It builds the clean environment and executes setpriv, which changes the user
// and starts `__toolenv`. It never runs PROGRAM itself: if setpriv cannot be started the
// command fails, and no path leads to PROGRAM as root.
//
// The security of this stage rests on this binary being static (the image checks it at build
// time and the platform preconditions at start), so no loader variable can affect it.
func runAstool(args []string) int {
	plan, err := sandbox.PlanAstool(stageLayout, args, os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "artex: "+err.Error())
		return sandbox.ExitCodeOf(err)
	}
	if plan.VarsFile != nil {
		// The variables travel on a descriptor that survives exec. It is a duplicate at 10 or
		// above, so nothing the runtime holds is replaced, and the second stage is told the number.
		fd, err := inheritableFD(plan.VarsFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "artex: cannot hand over the variables")
			return sandbox.ExitCannotRun
		}
		plan.SetVarsFD(fd)
	}
	if plan.LimitProcesses() {
		if err := limitToolProcesses(); err != nil {
			fmt.Fprintf(os.Stderr, "artex: cannot set the tool user's process limit: %v\n", err)
			return sandbox.ExitCannotRun
		}
	}
	err = syscall.Exec(plan.Path, plan.Argv, plan.Env)
	runtime.KeepAlive(plan.VarsFile)
	fmt.Fprintf(os.Stderr, "artex: cannot start %s: %v\n", plan.Path, err)
	return sandbox.ExitCannotRun
}

// inheritableFD duplicates f onto a descriptor of 10 or more that is not closed on exec.
func inheritableFD(f *os.File) (int, error) {
	n, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_DUPFD, 10)
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

// limitToolProcesses sets RLIMIT_NPROC to sandbox.ToolMaxProcesses (soft and hard; lower when the
// hard limit already is). It is inherited through setpriv by every process of the tool user this
// chain starts, and the tool user cannot raise it again.
func limitToolProcesses() error {
	var cur unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NPROC, &cur); err != nil {
		return err
	}
	n := uint64(sandbox.ToolMaxProcesses)
	if cur.Max < n {
		n = cur.Max
	}
	return unix.Setrlimit(unix.RLIMIT_NPROC, &unix.Rlimit{Cur: n, Max: n})
}
