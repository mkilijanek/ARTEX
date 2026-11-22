//go:build !windows

package main

import (
	"os"

	"github.com/mkilijanek/artex/sandbox"
)

// runStage runs the stage of the drop chain named by the first argument. ok is false when the
// argument names no stage and the server starts as usual.
func runStage(name string, args []string) (code int, ok bool) {
	switch name {
	case sandbox.CmdAstool:
		return runAstool(args), true
	case sandbox.CmdToolenv:
		return runToolenv(args), true
	case sandbox.CmdKillall:
		return runKillall(args), true
	case sandbox.CmdToolfs:
		return runToolfs(args), true
	case sandbox.CmdMigrateWorkspaces:
		return runMigrateWorkspaces(args, os.Stderr), true
	}
	return 0, false
}
