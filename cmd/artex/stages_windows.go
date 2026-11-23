//go:build windows

package main

import (
	"fmt"
	"os"

	"github.com/mkilijanek/artex/sandbox"
)

// runStage: the drop chain needs a Unix host. A stage name is refused rather than ignored, so
// the server never starts by accident with a stage's arguments.
func runStage(name string, args []string) (code int, ok bool) {
	switch name {
	case sandbox.CmdAstool, sandbox.CmdToolenv, sandbox.CmdKillall, sandbox.CmdToolfs, sandbox.CmdMigrateWorkspaces:
		fmt.Fprintln(os.Stderr, "artex: "+name+" is not available on this platform")
		return 1, true
	}
	return 0, false
}
