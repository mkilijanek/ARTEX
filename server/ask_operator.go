package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/sandbox"
)

const askOperatorMaxImage = 10 << 20

var askOperatorImageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true}

// toolAskOperator lets a worker formally request operator input (captcha, OTP,
// 2FA, a decision): the question and an optional screenshot land in the intent's
// transcript as a user-role turn with an attachment card, and the intent pauses.
// The operator replies through the existing message-to-worker flow; the reply
// resumes the run with the answer as the next input (#41).
func (s *Server) toolAskOperator() actool.CoreTool {
	return wrTool("ask_operator",
		"Ask the human operator for input you cannot obtain yourself (a captcha solution, an email/SMS OTP code, a 2FA answer, a decision, or an action only they can perform). The question appears in this intent's chat for the operator, optionally with a screenshot, and this intent then pauses; the operator's reply resumes this run with the answer as your next input. Ask precisely, and only when you are genuinely blocked. When the block is environmental (a sandboxed or filtered network path, a missing binary or key, a proxy or TLS failure this runtime cannot fix), do not retry or work around it: include in the question the exact single command the operator should run in their own terminal, as one copy-pastable line, plus what to paste back, then wait.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "What you need from the operator, precisely (e.g. 'enter the 6-digit code sent to x@y', 'solve the captcha in the attached screenshot'). For environmental blocks: the exact single command they should run in their own terminal (one line, copy-pastable) and what to paste back.",
				},
				"image_path": map[string]any{
					"type":        "string",
					"description": "Optional path to a screenshot (png/jpg/jpeg/webp/gif, max 10MB), e.g. the browser_take_screenshot output. It is copied into the task uploads and shown to the operator.",
				},
			},
			"required": []any{"question"},
		},
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var req struct {
				Question  string `json:"question"`
				ImagePath string `json:"image_path"`
			}
			_ = json.Unmarshal(in, &req)
			q := strings.TrimSpace(req.Question)
			if q == "" {
				return actool.Errorf("Error: question is required"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID == 0 || ri.IntentID == 0 {
				return actool.Errorf("Error: ask_operator can only be used inside a worker intent run"), nil
			}
			taskID := fmt.Sprintf("%d", ri.TaskID)
			t := s.m.ResolveTask(taskID)
			if t == nil {
				return actool.Errorf("Error: task not found"), nil
			}
			att, err := stageAskOperatorImage(s.m.dir, taskID, "i"+fmt.Sprint(ri.IntentID), req.ImagePath)
			if err != nil {
				return actool.Errorf("Error: " + err.Error()), nil
			}
			text := "OPERATOR INPUT REQUESTED: " + q + " (this intent is pausing; reply here to resume it with your answer)"
			act := userActivityWithAttachments("worker", text, att)
			node := ri.IntentID
			act.NodeID = &node
			s.engine.emitActivity(t, act)
			// The control call runs on a detached context: pausing may cancel the
			// run's context, and the pause bookkeeping itself must complete.
			if _, err := s.applyIntentControl(context.Background(), t, ri.IntentID, "pause", firstLine(q, 120), ""); err != nil {
				return actool.Errorf("Error: question delivered but pausing the intent failed: " + err.Error()), nil
			}
			return actool.Text("Question delivered to the operator. This intent is pausing now: finish your current step, record everything you already know as facts, and stop. The operator's reply will resume this run and arrive as your next input."), nil
		})
}

// stageAskOperatorImage copies the referenced image into the task's uploads dir
// (the same place chat uploads land, so the transcript attachment card resolves)
// and returns the attachment descriptor. Empty src returns no attachment.
//
// The path is the model's, and the workspace is writable by the tool user, so both sides go
// through os.Root:
//   - the source must lie in the task's working directory (relative paths are taken from workRel,
//     the intent's own directory, which is the tool's cwd) or in the browser MCP server's working
//     directory, and is opened relative to that directory; a link as
//     the last component is refused, one that leaves the directory fails in the root, and what was
//     opened must be a regular file with one name and at most askOperatorMaxImage bytes;
//   - the copy is created with O_EXCL (never through a link) under a random name in an uploads
//     directory that was checked as a directory of the workspace, and handed to the tool user.
func stageAskOperatorImage(dataDir, taskID, workRel, src string) ([]chatAttachment, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, nil
	}
	ext := strings.ToLower(filepath.Ext(src))
	if !askOperatorImageExt[ext] {
		return nil, fmt.Errorf("image_path must be a png/jpg/jpeg/webp/gif file, got %q", filepath.Base(src))
	}
	taskDir := filepath.Join(dataDir, "tasks", taskID)
	where, rel, err := imageSource(taskDir, workRel, src)
	if err != nil {
		return nil, err
	}
	var task *os.Root
	if where == imageInBrowserDir {
		// A screenshot the browser MCP server saved in its own working directory (the one the spawn
		// function made under the tool home). Each component is checked as a directory, not a link.
		home := browserDirBase()
		task, err = sandbox.OpenDirNoFollow(home, "mcp", mcpDirName(browserServerName))
	} else {
		fi, lerr := os.Lstat(taskDir)
		if lerr != nil {
			return nil, fmt.Errorf("image not readable: %v", lerr)
		}
		if !fi.IsDir() {
			return nil, errors.New("the task's working directory is not a directory")
		}
		task, err = os.OpenRoot(taskDir)
	}
	if err != nil {
		return nil, fmt.Errorf("image not readable: %v", err)
	}
	defer task.Close()
	in, size, err := openWorkspaceImage(task, rel)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	iso := sandbox.Current()
	uploads, err := iso.OpenWorkspace(taskDir, "uploads")
	if err != nil {
		return nil, err
	}
	defer uploads.Close()
	out, name, err := createUnique(uploads, "ask-operator-"+strconv.FormatInt(time.Now().Unix(), 10), ext)
	if err != nil {
		return nil, err
	}
	fail := func(err error) ([]chatAttachment, error) {
		out.Close()
		_ = uploads.Remove(name)
		return nil, err
	}
	n, err := io.Copy(out, io.LimitReader(in, askOperatorMaxImage+1))
	if err != nil {
		return fail(err)
	}
	if n > askOperatorMaxImage {
		return fail(fmt.Errorf("image too large (more than %s, max 10MB)", humanBytes(askOperatorMaxImage)))
	}
	if iso.On() && os.Geteuid() == 0 {
		if err := out.Chown(sandbox.ToolUID, sandbox.ToolGID); err != nil {
			return fail(err)
		}
	}
	if err := out.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = uploads.Remove(name)
		return nil, err
	}
	return []chatAttachment{{Name: name, Path: "uploads/" + name, Size: size}}, nil
}

// browserServerName is the seeded browser MCP server, whose working directory is the one place
// outside the task directory a screenshot may come from.
const browserServerName = "browser"

type imageWhere int

const (
	imageInTaskDir imageWhere = iota
	imageInBrowserDir
)

// browserDirBase is the tool home the spawn function puts MCP working directories under.
func browserDirBase() string {
	if h := sandbox.Current().Layout().ToolHome; h != "" {
		return h
	}
	return sandbox.ToolHome
}

// imageSource decides where the model's path lies and returns it relative to that directory. An
// absolute path must lie in the task directory or in the browser MCP server's working directory;
// a relative one is taken from taskDir/workRel. The check is lexical: it only decides what to ask
// which root for, and the root then refuses whatever leaves its directory.
func imageSource(taskDir, workRel, src string) (imageWhere, string, error) {
	abs := src
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(taskDir, workRel, src)
	}
	abs = filepath.Clean(abs)
	inside := func(dir string) (string, bool) {
		rel, err := filepath.Rel(dir, abs)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false
		}
		return rel, true
	}
	if rel, ok := inside(taskDir); ok {
		return imageInTaskDir, rel, nil
	}
	if rel, ok := inside(filepath.Join(browserDirBase(), "mcp", mcpDirName(browserServerName))); ok {
		return imageInBrowserDir, rel, nil
	}
	return 0, "", errors.New("image_path must be a file in this task's working directory or in the browser server's output directory")
}

// openWorkspaceImage opens rel in the task root and returns the file with its size, read from the
// open descriptor. os.Root follows a link that stays inside the root, even as the last component,
// so a link there is found with Lstat and refused, and the open file must be the one that was seen.
func openWorkspaceImage(task *os.Root, rel string) (*os.File, int64, error) {
	lfi, err := task.Lstat(rel)
	if err != nil {
		return nil, 0, fmt.Errorf("image not readable: %v", err)
	}
	if !lfi.Mode().IsRegular() {
		return nil, 0, errors.New("image_path must be a regular file, not a link or a directory")
	}
	f, err := task.OpenFile(rel, os.O_RDONLY|noFollowFlag|nonBlockFlag, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("image not readable: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("image not readable: %v", err)
	}
	if !st.Mode().IsRegular() || !os.SameFile(lfi, st) {
		f.Close()
		return nil, 0, errors.New("image_path changed while it was being opened")
	}
	if names, ok := linkCount(st); ok && names > 1 {
		f.Close()
		return nil, 0, errors.New("image_path is a file with more than one name")
	}
	if st.Size() > askOperatorMaxImage {
		f.Close()
		return nil, 0, fmt.Errorf("image too large (%s, max 10MB)", humanBytes(st.Size()))
	}
	return f, st.Size(), nil
}
