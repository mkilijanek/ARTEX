package intercept

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mkilijanek/artex/db"
)

func openAskDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v)", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	return pg
}

// captureLog collects the standard logger's output for the test.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	return func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// When the pending row cannot be created the answer stays deny, and the failure is logged with
// its error class and the tool name, capped, and without the tool input.
func TestHandleAskLogsAFailedPendingInsertAndDenies(t *testing.T) {
	pg := openAskDB(t)
	pg.Close() // every statement now fails
	logs := captureLog(t)
	i := New(pg)
	tool := "evil\nforged line " + strings.Repeat("t", 300)
	const secretInput = "CANARY-input-4c1d"
	if i.HandleAsk(context.Background(), 0, Decision{Action: "ask", TimeoutEnabled: true, TimeoutSeconds: 1, TimeoutAction: "allow"},
		tool, []byte(`{"k":"`+secretInput+`"}`)) {
		t.Fatal("HandleAsk allowed although the pending row could not be created")
	}
	out := logs()
	if !strings.Contains(out, "could not be recorded") || !strings.Contains(out, "denied") {
		t.Fatalf("no log line for the failed insert: %q", out)
	}
	if strings.Contains(out, "forged line\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("the tool name forged a log line: %q", out)
	}
	if len(out) > 400 || strings.Contains(out, secretInput) {
		t.Fatalf("log line not capped or carries the input (%d bytes): %q", len(out), out)
	}
}

// A timeout whose resolution fails is logged, the row cannot be marked, and the result is deny even
// when the timeout policy says allow.
func TestHandleAskLogsAFailedTimeoutResolutionAndDenies(t *testing.T) {
	pg := openAskDB(t)
	probe := openAskDB(t)
	defer probe.Close()
	tool := "t-timeout-" + time.Now().Format("150405.000000")
	t.Cleanup(func() { _, _ = probe.Exec(`DELETE FROM intercept_pending WHERE tool_name=$1`, tool) })
	logs := captureLog(t)
	i := New(pg)
	res := make(chan bool, 1)
	go func() {
		res <- i.HandleAsk(context.Background(), 0, Decision{Action: "ask", TimeoutEnabled: true, TimeoutSeconds: 1, TimeoutAction: "allow"}, tool, []byte(`{}`))
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := probe.QueryRow(`SELECT count(*) FROM intercept_pending WHERE tool_name=$1`, tool).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pending row never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pg.Close() // the timeout resolution now fails
	select {
	case allowed := <-res:
		if allowed {
			t.Fatal("a failed timeout resolution allowed the call")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("HandleAsk did not return after the timeout")
	}
	out := logs()
	if !strings.Contains(out, "timeout resolution failed") || !strings.Contains(out, tool) {
		t.Fatalf("the failed resolution is not logged with the tool name: %q", out)
	}
}

func TestLogSafeClipsAndSanitises(t *testing.T) {
	got := logSafe("a\nb\r\x00" + strings.Repeat("x", 200))
	if strings.ContainsAny(got, "\n\r\x00") || len(got) > 80 {
		t.Fatalf("got %q", got)
	}
}
