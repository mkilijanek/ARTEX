package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bbot keys its dependency state by host name, so a run in an operator's container without
// --no-deps prints "Installing" and tries to install at run time (found while building the image).
// Every invocation of bbot in a shipped skill must pass --no-deps. An invocation is a `bbot` command
// word with arguments other than --version / --help.
var (
	bbotCommand = regexp.MustCompile("(^|[\\s`(&;|])bbot(\\s+)(\\S.*)?$")
	codeSpan    = regexp.MustCompile("`([^`]+)`")
)

// invocations returns the code spans and fenced lines of a markdown or shell file that start a bbot
// command, split on shell separators so `bbot --version && bbot -l | head` is two commands.
func bbotInvocations(text string) []string {
	var out []string
	var candidates []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			candidates = append(candidates, line)
			continue
		}
		for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
			candidates = append(candidates, m[1])
		}
	}
	for _, c := range candidates {
		for _, part := range regexp.MustCompile(`&&|\|\||;|\|`).Split(c, -1) {
			part = strings.TrimSpace(part)
			if !strings.HasPrefix(part, "bbot ") && part != "bbot" {
				continue
			}
			out = append(out, part)
		}
	}
	return out
}

func TestBbotInvocationFinder(t *testing.T) {
	got := bbotInvocations("run `bbot --version && bbot -l | head -40` then\n```\nbbot -t x -m y\nls\n```\nand `-y --no-deps` alone and `bbot` words")
	want := []string{"bbot --version", "bbot -l", "bbot -t x -m y"}
	if len(got) < len(want) {
		t.Fatalf("got %q", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("invocation %d: %q, want %q", i, got[i], w)
		}
	}
}

func TestEveryBbotInvocationInTheSkillsPassesNoDeps(t *testing.T) {
	root := filepath.Join("..", "skills")
	seen := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch filepath.Ext(p) {
		case ".md", ".sh", ".txt", ".yaml", ".yml", ".json":
		default:
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, inv := range bbotInvocations(string(raw)) {
			args := strings.Fields(strings.TrimPrefix(inv, "bbot"))
			if len(args) == 0 || (len(args) == 1 && (args[0] == "--version" || args[0] == "--help" || args[0] == "-h")) {
				continue
			}
			seen++
			if !strings.Contains(" "+inv+" ", " --no-deps ") {
				t.Errorf("%s: bbot without --no-deps: %s", p, inv)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 8 {
		t.Errorf("only %d bbot invocations found: the finder lost the skill's examples", seen)
	}
}
