//go:build unix

package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mkilijanek/artex/sandbox"
)

type uploadEnv struct {
	s             *Server
	data, outside string
}

func newUploadEnv(t *testing.T) uploadEnv {
	t.Helper()
	data := t.TempDir()
	m := &Manager{dir: data, tasks: map[string]*Task{"7": {ID: "7"}}}
	return uploadEnv{s: &Server{m: m, engine: NewEngine(m)}, data: data, outside: t.TempDir()}
}

func (e uploadEnv) upload(t *testing.T, scope, id string, files map[string]string) (int, []chatAttachment) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, content := range files {
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte(content))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/chat/upload?scope="+scope+"&id="+id, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.s.chatUpload(rec, req)
	var out struct {
		Attachments []chatAttachment `json:"attachments"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Attachments
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChatUploadLandsInTheScopeDirectoryAndNeverClobbers(t *testing.T) {
	e := newUploadEnv(t)
	for scope, c := range map[string]struct{ dir, id string }{
		"task":    {"tasks", "7"},
		"session": {"sessions", "conv-3"},
		"staging": {"drafts", "d1"},
	} {
		code, att := e.upload(t, scope, c.id, map[string]string{"report.txt": "first"})
		if code != 200 || len(att) != 1 || att[0].Path != "uploads/report.txt" {
			t.Fatalf("%s: %d %+v", scope, code, att)
		}
		want := filepath.Join(e.data, c.dir, c.id, "uploads", "report.txt")
		if att[0].Abs != want || readFile(t, want) != "first" {
			t.Fatalf("%s: abs %q", scope, att[0].Abs)
		}
		code, att = e.upload(t, scope, c.id, map[string]string{"report.txt": "second"})
		if code != 200 || len(att) != 1 || att[0].Path != "uploads/report-1.txt" {
			t.Fatalf("%s again: %d %+v", scope, code, att)
		}
		if readFile(t, want) != "first" || readFile(t, filepath.Join(filepath.Dir(want), "report-1.txt")) != "second" {
			t.Errorf("%s: an upload replaced an earlier one", scope)
		}
	}
}

// A link planted in the workspace is not written through: at the file name, as a dangling link,
// at the uploads directory, or at the task directory.
func TestChatUploadDoesNotFollowLinksPlantedByATool(t *testing.T) {
	e := newUploadEnv(t)
	victim := filepath.Join(e.outside, "victim")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploads := filepath.Join(e.data, "tasks", "7", "uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(e.outside, "created-by-upload")
	for name, target := range map[string]string{"report.txt": victim, "ghost.txt": dangling} {
		if err := os.Symlink(target, filepath.Join(uploads, name)); err != nil {
			t.Fatal(err)
		}
	}
	code, att := e.upload(t, "task", "7", map[string]string{"report.txt": "NEW", "ghost.txt": "NEW"})
	if code != 200 || len(att) != 2 {
		t.Fatalf("%d %+v", code, att)
	}
	if got := readFile(t, victim); got != "ORIGINAL" {
		t.Errorf("the link target was overwritten: %q", got)
	}
	if _, err := os.Lstat(dangling); err == nil {
		t.Error("a file was created through a dangling link")
	}
	for _, a := range att {
		fi, err := os.Lstat(a.Abs)
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: not a regular file: %v %v", a.Name, fi, err)
		}
	}

	// the uploads directory is a link
	if err := os.RemoveAll(uploads); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.outside, uploads); err != nil {
		t.Fatal(err)
	}
	code, _ = e.upload(t, "task", "7", map[string]string{"x.txt": "NEW"})
	if code == 200 {
		t.Error("an upload through a linked uploads directory succeeded")
	}
	// the task directory is a link
	if err := os.RemoveAll(filepath.Join(e.data, "tasks", "7")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.outside, filepath.Join(e.data, "tasks", "7")); err != nil {
		t.Fatal(err)
	}
	code, _ = e.upload(t, "task", "7", map[string]string{"y.txt": "NEW"})
	if code == 200 {
		t.Error("an upload through a linked task directory succeeded")
	}
	ents, _ := os.ReadDir(e.outside)
	if len(ents) != 1 || ents[0].Name() != "victim" {
		t.Errorf("something was written outside: %v", ents)
	}
}

func TestChatUploadOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	activateEnforcedTree(t)
	e := newUploadEnv(t)
	own := func(p string) uint32 {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Sys().(*syscall.Stat_t).Uid
	}
	_, att := e.upload(t, "task", "7", map[string]string{"a.txt": "x"})
	if u := own(att[0].Abs); u != sandbox.ToolUID {
		t.Errorf("a task upload belongs to %d: the tools must be able to use it", u)
	}
	if u := own(filepath.Dir(att[0].Abs)); u != sandbox.ToolUID {
		t.Errorf("the uploads directory belongs to %d", u)
	}
	// drafts are the server's: readable by the tools, not writable, replaceable or removable by them
	_, att = e.upload(t, "staging", "d1", map[string]string{"a.txt": "x"})
	for _, p := range []string{att[0].Abs, filepath.Dir(att[0].Abs), filepath.Join(e.data, "drafts", "d1"), filepath.Join(e.data, "drafts")} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if own(p) != 0 || fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s: owner %d mode %v: a draft must not be writable by the tool user", p, own(p), fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(att[0].Abs); fi.Mode().Perm()&0o444 != 0o444 {
		t.Errorf("draft file mode %v: the model must be able to read it", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(e.data, "drafts")); fi.Mode().Perm() != 0o711 {
		t.Errorf("drafts mode %v", fi.Mode().Perm())
	}
}

func TestChatUploadStillRejectsBadInput(t *testing.T) {
	e := newUploadEnv(t)
	for _, q := range []string{"scope=other&id=7", "scope=task&id=../x", "scope=task&id=", "scope=task&id=99"} {
		req := httptest.NewRequest(http.MethodPost, "/api/chat/upload?"+q, strings.NewReader(""))
		rec := httptest.NewRecorder()
		e.s.chatUpload(rec, req)
		if rec.Code < 400 {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}
