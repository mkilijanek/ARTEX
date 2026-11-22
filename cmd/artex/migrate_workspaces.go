package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mkilijanek/artex/config"
	"github.com/mkilijanek/artex/sandbox"
)

// runMigrateWorkspaces is the runbook command `artex __migrate-workspaces [--data DIR] [--task ID]`:
// it finishes what a session start leaves when its bound is reached, with the same walk and no
// bound. It must run as root, with the server stopped (it refuses while the server's lock on the
// data directory is held, in this container or in another one sharing the volume).
//
// Exit codes: 0 done; 1 failed, or a workspace was refused or not finished; 2 usage; 3 the server
// is running.
func runMigrateWorkspaces(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet(sandbox.CmdMigrateWorkspaces, flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", filepath.Join(config.BaseDir(), "data"), "data directory")
	task := fs.String("task", "", "only the workspace of this task id (default: every task and chat session)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "artex: unexpected arguments")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "artex: "+sandbox.CmdMigrateWorkspaces+" must run as root")
		return 1
	}
	release, err := sandbox.LockDataDirExclusive(*data)
	switch {
	case errors.Is(err, sandbox.ErrServerRunning):
		fmt.Fprintln(stderr, "artex: a server is running against "+*data+"; stop it first (for example `docker compose stop artex`, then `docker compose run --rm --no-deps --entrypoint /app/artex artex "+sandbox.CmdMigrateWorkspaces+"`)")
		return 3
	case err != nil:
		fmt.Fprintf(stderr, "artex: %v\n", err)
		return 1
	}
	defer release()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	rep, err := sandbox.MigrateAllWorkspaces(ctx, *data, *task, logf)
	logf("[migrate-workspaces] %d workspaces done, %d refused or not finished, %d entries visited, %d handed to the tool user, %d hard-linked files left alone", rep.Workspaces, rep.Failed, rep.Entries, rep.Chowned, rep.HardLinks)
	if err != nil {
		fmt.Fprintf(stderr, "artex: %v\n", err)
		return 1
	}
	if rep.Failed > 0 {
		return 1
	}
	return 0
}
