package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The work dir is written by tools, so any component of a path can be replaced between the
// moment the API looks at it and the moment it opens it. These tests make that replacement at
// exactly that moment, through wsWalkHook, and check that the operation is refused and that the
// file the replacement points to (here key.txt and keys/, standing in for the server's files
// elsewhere in the data directory) is neither read nor changed.

const wsServerKey = "SERVER-KEY"

// wsSwapAt makes the walk hook run swap once, when the component at is reached.
func wsSwapAt(t *testing.T, at string, swap func()) *bool {
	t.Helper()
	done := new(bool)
	wsWalkHook = func(rel string) {
		if rel == at && !*done {
			*done = true
			swap()
		}
	}
	t.Cleanup(func() { wsWalkHook = nil })
	return done
}

// newWsRaceFixture adds a stand-in server file (key.txt), a stand-in server directory (keys/)
// and ordinary files the swaps replace: a.txt and d/a.txt.
func newWsRaceFixture(t *testing.T) *wsFixture {
	t.Helper()
	f := newWsFixture(t)
	for p, body := range map[string]string{"key.txt": wsServerKey, "keys/a.txt": wsServerKey, "a.txt": "ordinary", "d/a.txt": "ordinary"} {
		full := filepath.Join(f.ws, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// swapFileForLink replaces ws/name by a link to key.txt, which stays inside the work dir.
func (f *wsFixture) swapFileForLink(t *testing.T, name string) func() {
	return func() {
		p := filepath.Join(f.ws, name)
		if err := os.Remove(p); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("key.txt", p); err != nil {
			t.Error(err)
		}
	}
}

// swapDirForLink replaces ws/d by a link to keys/.
func (f *wsFixture) swapDirForLink(t *testing.T) func() {
	return func() {
		p := filepath.Join(f.ws, "d")
		if err := os.Rename(p, p+".moved"); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("keys", p); err != nil {
			t.Error(err)
		}
	}
}

func (f *wsFixture) serverFilesUnchanged(t *testing.T) {
	t.Helper()
	for _, p := range []string{"key.txt", "keys/a.txt"} {
		if b, err := os.ReadFile(filepath.Join(f.ws, p)); err != nil || string(b) != wsServerKey {
			t.Errorf("%s changed: %q %v", p, b, err)
		}
	}
	if ents, err := os.ReadDir(filepath.Join(f.ws, "keys")); err != nil || len(ents) != 1 {
		t.Errorf("keys/ gained or lost entries: %v %v", ents, err)
	}
}

func TestWorkspaceReadRefusesAPathSwappedAfterItsCheck(t *testing.T) {
	for _, tc := range []struct{ what, path, at string }{
		{"file", "a.txt", "a.txt"},
		{"directory", "d/a.txt", "d"},
	} {
		for _, ep := range []string{"read", "download"} {
			f := newWsRaceFixture(t)
			swap := f.swapFileForLink(t, "a.txt")
			if tc.what == "directory" {
				swap = f.swapDirForLink(t)
			}
			done := wsSwapAt(t, tc.at, swap)
			h := f.s.wsRead
			if ep == "download" {
				h = f.s.wsDownload
			}
			w := f.get(h, "/api/workspace/"+ep, tc.path)
			if !*done {
				t.Fatalf("%s %s: the swap did not run", ep, tc.what)
			}
			if w.Code == 200 || strings.Contains(w.Body.String(), wsServerKey) {
				t.Errorf("%s of a %s swapped after its check: status %d body %q", ep, tc.what, w.Code, w.Body.String())
			}
		}
	}
}

func TestWorkspaceWriteRefusesAPathSwappedAfterItsCheck(t *testing.T) {
	f := newWsRaceFixture(t)
	done := wsSwapAt(t, "a.txt", f.swapFileForLink(t, "a.txt"))
	if w := f.post(f.s.wsWrite, map[string]string{"path": "a.txt", "content": "PWNED"}); w.Code == 200 || !*done {
		t.Errorf("write over a file swapped for a link: status %d (swap ran: %v)", w.Code, *done)
	}
	f.serverFilesUnchanged(t)

	f = newWsRaceFixture(t)
	done = wsSwapAt(t, "d", f.swapDirForLink(t))
	if w := f.post(f.s.wsWrite, map[string]string{"path": "d/new.txt", "content": "PWNED"}); w.Code == 200 || !*done {
		t.Errorf("write below a directory swapped for a link: status %d (swap ran: %v)", w.Code, *done)
	}
	f.serverFilesUnchanged(t)

	f = newWsRaceFixture(t)
	done = wsSwapAt(t, "a.txt", f.swapFileForLink(t, "a.txt"))
	if w := f.upload("", "a.txt", "PWNED"); w.Code == 200 || !*done {
		t.Errorf("upload over a file swapped for a link: status %d (swap ran: %v)", w.Code, *done)
	}
	f.serverFilesUnchanged(t)

	f = newWsRaceFixture(t)
	done = wsSwapAt(t, "d", f.swapDirForLink(t))
	if w := f.upload("d", "new.txt", "PWNED"); w.Code == 200 || !*done {
		t.Errorf("upload into a directory swapped for a link: status %d (swap ran: %v)", w.Code, *done)
	}
	f.serverFilesUnchanged(t)
}

func TestWorkspaceDeleteRefusesAPathSwappedAfterItsCheck(t *testing.T) {
	f := newWsRaceFixture(t)
	done := wsSwapAt(t, "d", f.swapDirForLink(t))
	if w := f.del("d/a.txt"); w.Code == 200 || !*done {
		t.Errorf("delete below a directory swapped for a link: status %d (swap ran: %v)", w.Code, *done)
	}
	f.serverFilesUnchanged(t)
}

// A file with a second name may be a hard link to a server file: it is neither read nor written.
func TestWorkspaceRefusesFilesWithMoreThanOneName(t *testing.T) {
	f := newWsRaceFixture(t)
	if err := os.Link(filepath.Join(f.ws, "key.txt"), filepath.Join(f.ws, "hl.txt")); err != nil {
		t.Fatal(err)
	}
	for _, ep := range []string{"read", "download"} {
		h := f.s.wsRead
		if ep == "download" {
			h = f.s.wsDownload
		}
		if w := f.get(h, "/api/workspace/"+ep, "hl.txt"); w.Code == 200 || strings.Contains(w.Body.String(), wsServerKey) {
			t.Errorf("%s of a hard link: status %d body %q", ep, w.Code, w.Body.String())
		}
	}
	if w := f.post(f.s.wsWrite, map[string]string{"path": "hl.txt", "content": "PWNED"}); w.Code == 200 {
		t.Errorf("write over a hard link: status %d", w.Code)
	}
	if w := f.upload("", "hl.txt", "PWNED"); w.Code == 200 {
		t.Errorf("upload over a hard link: status %d", w.Code)
	}
	f.serverFilesUnchanged(t)
}
