package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MigrateAllReport sums an offline run.
type MigrateAllReport struct {
	Workspaces int // directories walked to the end
	Failed     int // directories refused (a link or a file where a directory belongs) or not walked to the end
	Entries    int
	Chowned    int
	HardLinks  int
}

// MigrateAllWorkspaces hands everything in the workspaces under dataDir (every directory directly
// in tasks/ and sessions/, or only tasks/<task> when task is not empty) to the tool user, with the
// same walk as a session start (MigrateWorkspace: relative to open directories, no link followed,
// hard-linked files left alone) but with no entry and no time bound, and with progress lines. The
// workspace directories themselves are made with EnsureWorkspace, so a link or a file where one
// belongs is refused and counted, not followed. It is for the runbook command, run with the
// server stopped; it does not check that (LockDataDirExclusive does).
func MigrateAllWorkspaces(ctx context.Context, dataDir, task string, logf func(string, ...any)) (MigrateAllReport, error) {
	var rep MigrateAllReport
	if logf == nil {
		logf = log.Printf
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return rep, err
	}
	cats := []string{"tasks", "sessions"}
	if task != "" {
		if _, err := strconv.ParseUint(task, 10, 63); err != nil {
			return rep, fmt.Errorf("task id %q is not a number", task)
		}
		cats = []string{"tasks"}
	}
	for _, cat := range cats {
		names, err := workspaceNames(filepath.Join(abs, cat), task)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return rep, fmt.Errorf("%s: %w", cat, err)
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			key := cat + "/" + name
			dir, err := EnsureWorkspace(filepath.Join(abs, cat), name)
			if err != nil {
				logf("[migrate-workspaces] %s: refused: %v", logName(key), err)
				rep.Failed++
				continue
			}
			opt := DefaultMigrateOptions()
			opt.MaxEntries, opt.MaxTime = 0, 0
			opt.StampPath, opt.StampKey, opt.Logf = filepath.Join(abs, ".isolation", "workspaces"), key, logf
			r, err := MigrateWorkspace(ctx, dir, opt)
			rep.Entries += r.Entries
			rep.Chowned += r.Chowned
			rep.HardLinks += r.HardLinks
			if err != nil {
				return rep, fmt.Errorf("%s: %w", key, err)
			}
			if r.Complete {
				rep.Workspaces++
			} else {
				rep.Failed++
				logf("[migrate-workspaces] %s: not walked to the end (%s)", logName(key), r.Reason)
			}
		}
	}
	return rep, nil
}

// workspaceNames lists the entries of a category directory (all of them, so a link or a file is
// reported by EnsureWorkspace), or only the named task.
func workspaceNames(catDir, only string) ([]string, error) {
	if only != "" {
		if _, err := os.Lstat(catDir); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(filepath.Join(catDir, only)); err != nil {
			return nil, err
		}
		return []string{only}, nil
	}
	ents, err := os.ReadDir(catDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			continue // staging directories of the server
		}
		out = append(out, e.Name())
	}
	return out, nil
}
