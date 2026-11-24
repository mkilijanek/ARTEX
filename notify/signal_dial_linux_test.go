//go:build linux

package notify

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dialOnce(t *testing.T, path string) error {
	t.Helper()
	conn, err := signalDialSocket(context.Background(), path)
	if err == nil {
		conn.Close()
	}
	return err
}

// Connecting through /proc/self/fd/<n> of an O_PATH descriptor reaches a unix socket.
func TestSignalDialSocketReachesARealSocket(t *testing.T) {
	dir := shortSockDir(t)
	sock := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan struct{}, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
			accepted <- struct{}{}
		}
	}()
	if err := dialOnce(t, sock); err != nil {
		t.Fatalf("dial of a real socket: %v", err)
	}
	<-accepted
}

// A symlink, a regular file and a missing path are refused, and the symlink target is never connected to.
func TestSignalDialSocketRefusals(t *testing.T) {
	dir := shortSockDir(t)
	sock := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conns := make(chan struct{}, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
			conns <- struct{}{}
		}
	}()
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.sock")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{link, "symlink"}, {plain, "not a socket"}, {dir, "not a socket"}, {filepath.Join(dir, "none"), signalUnreachable},
	} {
		err := dialOnce(t, c.path)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", filepath.Base(c.path), err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), dir) {
			t.Errorf("error leaks the path: %v", err)
		}
	}
	if len(conns) != 0 {
		t.Fatalf("the listener saw %d connection(s) from refused dials", len(conns))
	}
}

// A socket that is replaced by a symlink between two dials is refused the second time.
func TestSignalDialSocketRefusesASwappedPath(t *testing.T) {
	dir := shortSockDir(t)
	sock := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.sock")
	l2, err := net.Listen("unix", other)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	seen := make(chan struct{}, 4)
	go func() {
		for {
			c, err := l2.Accept()
			if err != nil {
				return
			}
			c.Close()
			seen <- struct{}{}
		}
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := dialOnce(t, sock); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	l.Close() // closing a unix listener removes its path
	if err := os.Symlink(other, sock); err != nil {
		t.Fatal(err)
	}
	if err := dialOnce(t, sock); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("second dial: err = %v, want the symlink refusal", err)
	}
	if len(seen) != 0 {
		t.Fatal("the swapped target was connected to")
	}
}
