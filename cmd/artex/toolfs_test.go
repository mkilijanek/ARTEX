//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/sandbox"
)

// toolfs runs `artex __astool [--cd dir] -- artex __toolfs` with the request on stdin, as the server
// does, through the chain of stand-in stages.
func (c *chain) toolfs(t *testing.T, dir string, request string, extra ...string) (stdout, stderr string, code int) {
	t.Helper()
	args := []string{sandbox.CmdAstool}
	if dir != "" {
		args = append(args, "--cd", dir)
	}
	args = append(args, "--", c.bin, sandbox.CmdToolfs)
	args = append(args, extra...)
	cmd := exec.Command(c.bin, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "NORMA_RIPGREP_NO_INSTALL=1"}
	cmd.Stdin = strings.NewReader(request)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

type toolfsAnswer struct {
	Texts   []string `json:"texts"`
	IsError bool     `json:"is_error"`
	Error   string   `json:"error"`
}

func (c *chain) call(t *testing.T, dir, tool string, input any) toolfsAnswer {
	t.Helper()
	in, _ := json.Marshal(input)
	req, _ := json.Marshal(map[string]any{"tool": tool, "input": in, "working_dir": dir, "max_output_chars": 5000})
	out, errOut, code := c.toolfs(t, dir, string(req))
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	var a toolfsAnswer
	if err := json.Unmarshal([]byte(out), &a); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	return a
}

// The helper is a stage of the binary: through the drop chain it reads a request, runs the SDK tool
// in the working directory the chain changed into, and answers on stdout.
func TestToolfsStageServesAFileToolThroughTheChain(t *testing.T) {
	c := newChain(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo needle\n"), 0o644)

	a := c.call(t, dir, "Read", map[string]any{"file_path": "a.txt"})
	if a.IsError || len(a.Texts) != 1 || !strings.Contains(a.Texts[0], "two needle") {
		t.Fatalf("Read: %+v", a)
	}
	a = c.call(t, dir, "Write", map[string]any{"file_path": "sub/b.txt", "content": "made by the helper"})
	if a.IsError || !strings.Contains(a.Texts[0], "Created sub/b.txt") {
		t.Fatalf("Write: %+v", a)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "sub", "b.txt")); err != nil || string(b) != "made by the helper" {
		t.Fatalf("the file: %q %v", b, err)
	}
	a = c.call(t, dir, "Grep", map[string]any{"pattern": "needle", "output_mode": "files_with_matches"})
	if a.IsError || !strings.Contains(a.Texts[0], "a.txt") {
		t.Fatalf("Grep: %+v", a)
	}
	a = c.call(t, dir, "Read", map[string]any{"file_path": "missing"})
	if !a.IsError || !strings.Contains(a.Texts[0], "Error reading missing") {
		t.Fatalf("a tool error is an answer, not a crash: %+v", a)
	}
}

// The request is data: a tool that is not a file tool is refused, whatever the caller asked.
func TestToolfsStageRefusesWhatIsNotAFileTool(t *testing.T) {
	c := newChain(t)
	for _, req := range []string{
		`{"tool":"Bash","input":"e30=","working_dir":"/","max_output_chars":10}`,
		`{"tool":"Read","input":"e30=","unknown":1}`,
		`not json`,
		``,
	} {
		out, errOut, code := c.toolfs(t, "", req)
		if code == 0 || out != "" || !strings.Contains(errOut, "artex:") {
			t.Errorf("request %q: exit %d out %q err %q", req, code, out, errOut)
		}
	}
	// no arguments are accepted
	if _, errOut, code := c.toolfs(t, "", `{}`, "--cd", "/"); code != 64 || !strings.Contains(errOut, "no arguments") {
		t.Errorf("arguments: exit %d %q", code, errOut)
	}
}

// A working directory the tool user cannot enter ends the stage before the tool runs.
func TestToolfsStageNeedsItsWorkingDirectory(t *testing.T) {
	c := newChain(t)
	out, errOut, code := c.toolfs(t, filepath.Join(t.TempDir(), "gone"), `{"tool":"LS","input":"e30="}`)
	if code != sandbox.ExitCannotRun || out != "" || !strings.Contains(errOut, "cannot change to") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
}
