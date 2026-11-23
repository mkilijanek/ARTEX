//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/mkilijanek/artex/sandbox"
)

// runToolenv is the second stage, started by setpriv as the tool user: `artex __toolenv [--cd
// DIR] [--vars-fd N] -- PROGRAM ARGS...`. It reads the per-call variables from descriptor N when
// the root stage handed one over, closes it, changes into DIR and executes PROGRAM with the
// clean environment plus those variables and SHELL=/bin/bash.
//
// A refused variable ends it with exit code 97 and no output: there is no stderr channel back to
// the server for an MCP child, so the server validates the keys and logs before it writes the
// file, and this check is the second line.
func runToolenv(args []string) int {
	plan, err := sandbox.PlanToolenv(args, os.Environ(), openVarsFD)
	if err != nil {
		if sandbox.ExitCodeOf(err) != sandbox.ExitDeniedVar {
			fmt.Fprintln(os.Stderr, "artex: "+err.Error())
		}
		return sandbox.ExitCodeOf(err)
	}
	if plan.Dir != "" {
		if err := os.Chdir(plan.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "artex: cannot change to %s: %v\n", plan.Dir, err)
			return sandbox.ExitCannotRun
		}
	}
	err = syscall.Exec(plan.Path, plan.Argv, plan.Env)
	fmt.Fprintf(os.Stderr, "artex: cannot start %s: %v\n", plan.Path, err)
	return sandbox.ExitCannotRun
}

// openVarsFD returns the descriptor the root stage named, which must be an open regular file.
func openVarsFD(fd int) (io.ReadCloser, error) {
	f := os.NewFile(uintptr(fd), "vars")
	if f == nil {
		return nil, errors.New("no such descriptor")
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("descriptor is not a regular file")
	}
	return f, nil
}
