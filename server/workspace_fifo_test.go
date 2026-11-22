//go:build unix

package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// within runs op and fails the test when it does not return in time. A handler stuck in the open
// of a FIFO is released at the end of the test by opening the FIFO for writing.
func within(t *testing.T, what string, fifos []string, op func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- op() }()
	select {
	case w := <-got:
		return w
	case <-time.After(3 * time.Second):
		for _, p := range fifos {
			if fd, err := syscall.Open(p, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				syscall.Close(fd)
			}
		}
		t.Fatalf("%s: the handler hangs on a FIFO", what)
		return nil
	}
}

// A FIFO planted in the work dir, or put in place of a file after its check, never blocks a
// handler: it is refused like any other entry that is not a regular file.
func TestWorkspaceHandlersDoNotHangOnAFIFO(t *testing.T) {
	f := newWsRaceFixture(t)
	p := filepath.Join(f.ws, "p")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
	fifos := []string{p, filepath.Join(f.ws, "a.txt")}
	for what, op := range map[string]func() *httptest.ResponseRecorder{
		"read":     func() *httptest.ResponseRecorder { return f.get(f.s.wsRead, "/api/workspace/read", "p") },
		"download": func() *httptest.ResponseRecorder { return f.get(f.s.wsDownload, "/api/workspace/download", "p") },
		"list":     func() *httptest.ResponseRecorder { return f.get(f.s.wsList, "/api/workspace/list", "p") },
		"write": func() *httptest.ResponseRecorder {
			return f.post(f.s.wsWrite, map[string]string{"path": "p", "content": "x"})
		},
		"upload": func() *httptest.ResponseRecorder { return f.upload("", "p", "x") },
		"mkdir":  func() *httptest.ResponseRecorder { return f.post(f.s.wsMkdir, map[string]string{"path": "p/x"}) },
	} {
		if w := within(t, what, fifos, op); w.Code == 200 {
			t.Errorf("%s of a FIFO: status 200", what)
		}
	}
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the FIFO was replaced: %v %v", fi, err)
	}

	for _, ep := range []string{"read", "download", "write"} {
		g := newWsRaceFixture(t)
		q := filepath.Join(g.ws, "a.txt")
		fifos = []string{q}
		wsSwapAt(t, "a.txt", func() {
			if err := os.Remove(q); err != nil {
				t.Error(err)
			}
			if err := syscall.Mkfifo(q, 0o644); err != nil {
				t.Error(err)
			}
		})
		op := func() *httptest.ResponseRecorder { return g.get(g.s.wsRead, "/api/workspace/read", "a.txt") }
		switch ep {
		case "download":
			op = func() *httptest.ResponseRecorder { return g.get(g.s.wsDownload, "/api/workspace/download", "a.txt") }
		case "write":
			op = func() *httptest.ResponseRecorder {
				return g.post(g.s.wsWrite, map[string]string{"path": "a.txt", "content": "x"})
			}
		}
		if w := within(t, ep+" of a file swapped for a FIFO", fifos, op); w.Code == 200 {
			t.Errorf("%s of a file swapped for a FIFO: status 200", ep)
		}
	}
}
