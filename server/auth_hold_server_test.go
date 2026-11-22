package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A client that sends a header announcing a body and then stalls is cut off by the per-request
// read deadline, over a real connection, and meanwhile holds nothing.
func TestStalledBodyOverARealConnectionIsCutOffAndHoldsNothing(t *testing.T) {
	useThrottle(t)
	useLocalAddrs(t, time.Now, fixedAddrs())
	g := newLoginRig(t)
	old := authBodyDeadline
	authBodyDeadline = 400 * time.Millisecond
	t.Cleanup(func() { authBodyDeadline = old })
	ts := httptest.NewServer(http.HandlerFunc(g.s.authLogin))
	t.Cleanup(ts.Close)

	var conns []net.Conn
	for i := 0; i < throttleFreeFailures; i++ {
		c, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if _, err := c.Write([]byte("POST /api/auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	time.Sleep(100 * time.Millisecond)
	b, _ := json.Marshal(map[string]string{"username": "ARTEX", "password": rigPassword})
	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("a complete correct login while %d bodies stall: status=%d want 200", len(conns), resp.StatusCode)
	}
	for i, c := range conns {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "HTTP/1.1 400") {
			t.Fatalf("stalled connection %d: reply %q err=%v, want a 400 after the read deadline", i, line, err)
		}
	}
}
