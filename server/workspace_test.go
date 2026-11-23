package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wsFixture builds a work dir with planted links and a separate "outside" directory that the
// API must never touch: secret.txt (content outsideSecret), and a victim file to delete.
type wsFixture struct {
	s       *Server
	ws      string
	outside string
}

const outsideSecret = "OUTSIDE-SECRET"

func newWsFixture(t *testing.T) *wsFixture {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{ws, outside, filepath.Join(ws, "real")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(outsideSecret), 0o644))
	must(os.WriteFile(filepath.Join(outside, "victim.txt"), []byte("victim"), 0o644))
	must(os.WriteFile(filepath.Join(ws, "real", "inner.txt"), []byte("inner"), 0o644))
	must(os.Symlink(outside, filepath.Join(ws, "l")))                                  // directory link out of the tree
	must(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ws, "f")))     // file link out of the tree
	must(os.Symlink(filepath.Join(outside, "missing.txt"), filepath.Join(ws, "dang"))) // dangling link out of the tree
	must(os.Symlink(filepath.Join(ws, "real"), filepath.Join(ws, "in")))               // directory link that stays inside
	return &wsFixture{s: &Server{m: &Manager{dir: ws}}, ws: ws, outside: outside}
}

func (f *wsFixture) get(h http.HandlerFunc, path string, q string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", path+"?path="+url.QueryEscape(q), nil))
	return w
}

func (f *wsFixture) del(q string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.s.wsDelete(w, httptest.NewRequest("DELETE", "/api/workspace/delete?path="+url.QueryEscape(q), nil))
	return w
}

func (f *wsFixture) post(h http.HandlerFunc, body map[string]string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", "/x", bytes.NewReader(b)))
	return w
}

func (f *wsFixture) upload(dir, name, content string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write([]byte(content))
	mw.Close()
	r := httptest.NewRequest("POST", "/api/workspace/upload?path="+url.QueryEscape(dir), &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	f.s.wsUpload(w, r)
	return w
}

func wantCode(t *testing.T, what string, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("%s: status=%d want %d; body=%s", what, w.Code, code, w.Body.String())
	}
}

func (f *wsFixture) outsideUnchanged(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.outside, "secret.txt"))
	if err != nil || string(b) != outsideSecret {
		t.Fatalf("outside secret changed: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(f.outside, "victim.txt")); err != nil {
		t.Fatalf("outside victim gone: %v", err)
	}
	ents, _ := os.ReadDir(f.outside)
	if len(ents) != 2 {
		t.Fatalf("outside directory gained or lost entries: %v", ents)
	}
}

func (f *wsFixture) linksRemain(t *testing.T) {
	t.Helper()
	for _, n := range []string{"l", "f", "dang", "in"} {
		if fi, err := os.Lstat(filepath.Join(f.ws, n)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("link %s did not stay in place: %v", n, err)
		}
	}
}

func TestWorkspaceReadRefusesLinks(t *testing.T) {
	f := newWsFixture(t)
	for _, p := range []string{"l/secret.txt", "f", "in/inner.txt", "dang"} {
		w := f.get(f.s.wsRead, "/api/workspace/read", p)
		wantCode(t, "read "+p, w, 400)
		if strings.Contains(w.Body.String(), outsideSecret) || strings.Contains(w.Body.String(), "inner") {
			t.Fatalf("read %s leaked content: %s", p, w.Body.String())
		}
	}
	f.outsideUnchanged(t)
	f.linksRemain(t)
}

func TestWorkspaceDownloadRefusesLinks(t *testing.T) {
	f := newWsFixture(t)
	for _, p := range []string{"l/secret.txt", "f", "in/inner.txt"} {
		w := f.get(f.s.wsDownload, "/api/workspace/download", p)
		wantCode(t, "download "+p, w, 400)
		if strings.Contains(w.Body.String(), outsideSecret) {
			t.Fatalf("download %s leaked content", p)
		}
	}
	f.linksRemain(t)
}

func TestWorkspaceWriteRefusesLinks(t *testing.T) {
	f := newWsFixture(t)
	for _, p := range []string{"l/new.txt", "l/sub/new.txt", "f", "dang", "in/new.txt"} {
		wantCode(t, "write "+p, f.post(f.s.wsWrite, map[string]string{"path": p, "content": "PWNED"}), 400)
	}
	f.outsideUnchanged(t)
	if _, err := os.Stat(filepath.Join(f.ws, "real", "new.txt")); err == nil {
		t.Fatal("wrote through an internal link")
	}
	f.linksRemain(t)
}

func TestWorkspaceMkdirRefusesLinks(t *testing.T) {
	f := newWsFixture(t)
	for _, p := range []string{"l/d", "l/a/b", "in/d", "f/d"} {
		w := f.post(f.s.wsMkdir, map[string]string{"path": p})
		if w.Code == 200 {
			t.Fatalf("mkdir %s succeeded", p)
		}
	}
	wantCode(t, "mkdir l/d", f.post(f.s.wsMkdir, map[string]string{"path": "l/d"}), 400)
	f.outsideUnchanged(t)
	f.linksRemain(t)
}

func TestWorkspaceDeleteRefusesLinksInThePath(t *testing.T) {
	f := newWsFixture(t)
	wantCode(t, "delete l/victim.txt", f.del("l/victim.txt"), 400)
	wantCode(t, "delete l/secret.txt", f.del("l/secret.txt"), 400)
	wantCode(t, "delete in/inner.txt", f.del("in/inner.txt"), 400)
	f.outsideUnchanged(t)
	f.linksRemain(t)
	if _, err := os.Stat(filepath.Join(f.ws, "real", "inner.txt")); err != nil {
		t.Fatalf("file behind an internal link was deleted: %v", err)
	}
}

func TestWorkspaceDeleteRemovesALinkAsALinkAndNeverItsTarget(t *testing.T) {
	f := newWsFixture(t)
	wantCode(t, "delete l", f.del("l"), 200)
	if _, err := os.Lstat(filepath.Join(f.ws, "l")); err == nil {
		t.Fatal("the link itself should have been removed")
	}
	f.outsideUnchanged(t)

	// A directory tree that contains a link: the link goes, the target stays.
	if err := os.MkdirAll(filepath.Join(f.ws, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outside, filepath.Join(f.ws, "tree", "out")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "delete tree", f.del("tree"), 200)
	f.outsideUnchanged(t)
}

func TestWorkspaceListRefusesLinkedDirectoriesAndShowsLinksAsEntries(t *testing.T) {
	f := newWsFixture(t)
	for _, p := range []string{"l", "in"} {
		w := f.get(f.s.wsList, "/api/workspace/list", p)
		wantCode(t, "list "+p, w, 400)
		if strings.Contains(w.Body.String(), "secret.txt") {
			t.Fatalf("list %s leaked entries of the target", p)
		}
	}
	w := f.get(f.s.wsList, "/api/workspace/list", "")
	wantCode(t, "list root", w, 200)
	var out struct {
		Entries []wsEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, e := range out.Entries {
		if (e.Name == "l" || e.Name == "in") && e.Dir {
			t.Fatalf("link %s is listed as a directory", e.Name)
		}
	}
}

func TestWorkspaceUploadRefusesLinks(t *testing.T) {
	f := newWsFixture(t)
	wantCode(t, "upload into l", f.upload("l", "up.txt", "PWNED"), 400)
	wantCode(t, "upload over f", f.upload("", "f", "PWNED"), 400)
	wantCode(t, "upload over dang", f.upload("", "dang", "PWNED"), 400)
	f.outsideUnchanged(t)
	f.linksRemain(t)
}

func TestWorkspaceOrdinaryOperationsStillWork(t *testing.T) {
	f := newWsFixture(t)
	wantCode(t, "write", f.post(f.s.wsWrite, map[string]string{"path": "/a/b/c.txt", "content": "hello"}), 200)
	w := f.get(f.s.wsRead, "/api/workspace/read", "a/b/c.txt")
	wantCode(t, "read", w, 200)
	if !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Fatalf("read body: %s", w.Body.String())
	}
	wantCode(t, "overwrite", f.post(f.s.wsWrite, map[string]string{"path": "a/b/c.txt", "content": "again"}), 200)
	wantCode(t, "mkdir", f.post(f.s.wsMkdir, map[string]string{"path": "x/y"}), 200)
	wantCode(t, "list", f.get(f.s.wsList, "/api/workspace/list", "a/b"), 200)
	w = f.get(f.s.wsDownload, "/api/workspace/download", "a/b/c.txt")
	wantCode(t, "download", w, 200)
	if w.Body.String() != "again" || !strings.Contains(w.Header().Get("Content-Disposition"), "c.txt") {
		t.Fatalf("download: body=%q header=%q", w.Body.String(), w.Header().Get("Content-Disposition"))
	}
	wantCode(t, "upload", f.upload("x/y", "u.txt", "up"), 200)
	if b, err := os.ReadFile(filepath.Join(f.ws, "x", "y", "u.txt")); err != nil || string(b) != "up" {
		t.Fatalf("uploaded file: %q %v", b, err)
	}
	wantCode(t, "write over a directory", f.post(f.s.wsWrite, map[string]string{"path": "a", "content": "x"}), 400)
	wantCode(t, "write at the root", f.post(f.s.wsWrite, map[string]string{"path": "/", "content": "x"}), 400)
	wantCode(t, "traversal read", f.get(f.s.wsRead, "/api/workspace/read", "../outside/secret.txt"), 404)
	wantCode(t, "delete", f.del("a"), 200)
	wantCode(t, "delete root", f.del(""), 400)
	wantCode(t, "read missing", f.get(f.s.wsRead, "/api/workspace/read", "nope"), 404)
	f.outsideUnchanged(t)
}
