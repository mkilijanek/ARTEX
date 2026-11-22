//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/mkilijanek/artex/sandbox"
)

// runKillall ends every process of the tool user, itself included, with kill(-1, SIGKILL). It is
// started through the drop chain, so it runs as the tool user; it refuses to run as anyone else,
// because as root the same call would end every process in the container. Ending by SIGKILL is
// its success: the caller treats that exit as done.
func runKillall(args []string) int {
	if err := sandbox.CheckKillallIdentity(os.Getuid(), os.Geteuid()); err != nil {
		fmt.Fprintln(os.Stderr, "artex: "+err.Error())
		return 1
	}
	if err := syscall.Kill(-1, syscall.SIGKILL); err != nil {
		fmt.Fprintf(os.Stderr, "artex: kill: %v\n", err)
		return 1
	}
	return 0
}
