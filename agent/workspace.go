package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mkilijanek/artex/sandbox"
)

// Every agent kind works in a directory of the data directory: tasks/<id> (planner, main agent),
// tasks/<id>/i<intent> (worker), sessions/<id> (chat). They are made here and nowhere else.

// runDirPath is the working directory of a planner or main agent (intentID 0) or a worker.
func runDirPath(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	return dir
}

// prepareRunDir makes the working directory of a planner, main agent or worker and, with
// isolation on, hands what is in it to the tool user.
func prepareRunDir(ctx context.Context, base string, taskID, intentID int64) (string, error) {
	rel := []string{strconv.FormatInt(taskID, 10)}
	if intentID > 0 {
		rel = append(rel, "i"+strconv.FormatInt(intentID, 10))
	}
	return prepareWorkspace(ctx, base, "tasks", rel...)
}

// prepareSessionDir makes the working directory of a chat session.
func prepareSessionDir(ctx context.Context, base, sessionID string) (string, error) {
	return prepareWorkspace(ctx, base, "sessions", sessionID)
}

// prepareWorkspace makes <base>/<category>/<rel...>. The category directory is the server's (0711);
// the workspace and what leads to it are the tool user's (0700), made component by component
// without following a link. A link or a file where a directory belongs fails the session start
// with a logged reason.
//
// With isolation on, everything in the workspace that is not the tool user's is then handed over
// (sandbox.MigrateWorkspace): files a 0.5.17m server left, root's files from a rolled-back run, a
// restored archive. It is bounded and never fails the start for reaching its bound.
func prepareWorkspace(ctx context.Context, base, category string, rel ...string) (string, error) {
	dir, err := makeWorkspace(ctx, base, category, rel)
	if err != nil {
		log.Printf("[workspace] session start refused: %v", err)
		return "", fmt.Errorf("workspace %s/%s: %w", category, strings.Join(rel, "/"), err)
	}
	return dir, nil
}

func makeWorkspace(ctx context.Context, base, category string, rel []string) (string, error) {
	abs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	// The data directory is the server's own and may not exist yet (a source install, a test);
	// everything below it is made without following a link.
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return "", err
	}
	cat, err := ensureServerDir(root, category, 0o711)
	root.Close()
	if err != nil {
		return "", fmt.Errorf("%s: %w", category, err)
	}
	cat.Close()
	catPath := filepath.Join(abs, category)

	iso := sandbox.Current()
	dir, err := iso.EnsureWorkspace(catPath, rel...)
	if err != nil {
		return "", err
	}
	if !iso.On() {
		return dir, nil
	}
	opt := sandbox.DefaultMigrateOptions()
	opt.StampPath = filepath.Join(abs, ".isolation", "workspaces")
	opt.StampKey = category + "/" + strings.Join(rel, "/")
	if _, err := sandbox.MigrateWorkspace(ctx, dir, opt); err != nil {
		return "", err
	}
	return dir, nil
}
