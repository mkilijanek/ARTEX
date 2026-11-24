package signalsidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// pipeServer is the far end of a Client: it reads the request lines the client
// writes and writes whatever lines a test hands it.
type pipeServer struct {
	t     *testing.T
	in    *bufio.Reader // requests from the client
	outW  *io.PipeWriter
	inW   *io.PipeWriter
	outMu sync.Mutex
}

func newPipeClient(t *testing.T, opts ClientOptions) (*Client, *pipeServer) {
	t.Helper()
	reqR, reqW := io.Pipe() // client -> server
	respR, respW := io.Pipe()
	c := NewClient(respR, reqW, opts)
	s := &pipeServer{t: t, in: bufio.NewReader(reqR), outW: respW, inW: reqW}
	t.Cleanup(func() {
		c.Close()
		respW.Close()
		reqR.Close()
	})
	return c, s
}

type reqLine struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *pipeServer) readReq() reqLine {
	s.t.Helper()
	line, err := s.in.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("server read: %v", err)
	}
	var r reqLine
	if err := json.Unmarshal(line, &r); err != nil {
		s.t.Fatalf("bad request line %q: %v", line, err)
	}
	return r
}

func (s *pipeServer) writeLine(line string) {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := io.WriteString(s.outW, line+"\n"); err != nil {
		s.t.Errorf("server write: %v", err)
	}
}

func (s *pipeServer) result(id json.RawMessage, result string) {
	s.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","result":%s,"id":%s}`, result, id))
}

func TestCallDecodesResultAndSendsAProperRequest(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() {
		r := s.readReq()
		if r.Method != "version" {
			t.Errorf("method %q", r.Method)
		}
		s.result(r.ID, `{"version":"0.14.10"}`)
	}()
	var out struct{ Version string }
	if err := c.Call(context.Background(), "version", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Version != "0.14.10" {
		t.Fatalf("got %+v", out)
	}
}

func TestCallSendsParamsAsAnObject(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	got := make(chan reqLine, 1)
	go func() {
		r := s.readReq()
		got <- r
		s.result(r.ID, `{}`)
	}()
	if err := c.Call(context.Background(), "finishLink", map[string]any{"deviceLinkUri": "u", "deviceName": "n"}, nil); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if string(r.Params) != `{"deviceLinkUri":"u","deviceName":"n"}` {
		t.Fatalf("params %s", r.Params)
	}
}

func TestConcurrentCallsAreMatchedByIDEvenWhenAnsweredOutOfOrder(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	const n = 40
	go func() {
		reqs := make([]reqLine, 0, n)
		for i := 0; i < n; i++ {
			reqs = append(reqs, s.readReq())
		}
		for i := len(reqs) - 1; i >= 0; i-- { // reverse order
			s.result(reqs[i].ID, string(reqs[i].Params))
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out int
			if err := c.Call(context.Background(), "echo", map[string]int{"v": i}, &struct {
				V *int `json:"v"`
			}{&out}); err != nil {
				t.Errorf("call %d: %v", i, err)
				return
			}
			if out != i {
				t.Errorf("call %d got the answer of %d", i, out)
			}
		}(i)
	}
	wg.Wait()
}

func TestCallTimeoutIsPerRequestAndALateAnswerIsIgnored(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	first := make(chan reqLine, 1)
	go func() { first <- s.readReq() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Call(ctx, "slow", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	// The answer arrives after the caller gave up; the next call still works.
	r := <-first
	go func() {
		s.result(r.ID, `"late"`)
		r2 := s.readReq()
		s.result(r2.ID, `"fresh"`)
	}()
	var out string
	if err := c.Call(context.Background(), "next", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out != "fresh" {
		t.Fatalf("got %q", out)
	}
	if c.Pending() != 0 {
		t.Fatalf("pending = %d", c.Pending())
	}
}

func TestOversizeLineIsSkippedAndReadingContinues(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{MaxLine: 1 << 20})
	go func() {
		r := s.readReq()
		// 5 MiB line, then the real answer.
		s.writeLine(`{"jsonrpc":"2.0","method":"receive","params":{"pad":"` + strings.Repeat("x", 5<<20) + `"}}`)
		s.result(r.ID, `"ok"`)
	}()
	var out string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Call(ctx, "m", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Fatalf("got %q", out)
	}
	if c.Skipped() != 1 {
		t.Fatalf("skipped = %d, want 1", c.Skipped())
	}
}

func TestLineAtTheLimitIsKeptAndOneByteMoreIsSkipped(t *testing.T) {
	const max = 4096
	c, s := newPipeClient(t, ClientOptions{MaxLine: max})
	go func() {
		r := s.readReq()
		prefix := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":"`, r.ID)
		suffix := `"}`
		pad := max - len(prefix) - len(suffix)
		s.writeLine(prefix + strings.Repeat("a", pad+1) + suffix) // one byte too long: skipped
		s.writeLine(prefix + strings.Repeat("a", pad) + suffix)   // exactly the limit: kept
	}()
	var out string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, "m", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || c.Skipped() != 1 {
		t.Fatalf("len(out)=%d skipped=%d", len(out), c.Skipped())
	}
}

func TestNotificationsAreDroppedAndCounted(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() {
		r := s.readReq()
		s.writeLine(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"dataMessage":{"message":"SENTINEL-BODY"}}}}`)
		s.writeLine(`{"jsonrpc":"2.0","method":"receive","params":{}}`)
		s.result(r.ID, `1`)
	}()
	var out int
	if err := c.Call(context.Background(), "m", nil, &out); err != nil {
		t.Fatal(err)
	}
	if c.Notifications() != 2 {
		t.Fatalf("notifications = %d", c.Notifications())
	}
}

func TestUnknownIDsAndGarbageLinesAreIgnored(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() {
		r := s.readReq()
		s.writeLine(`not json at all`)
		s.writeLine(``)
		s.writeLine(`{"jsonrpc":"2.0","result":1,"id":99999}`)
		s.writeLine(`{"jsonrpc":"2.0","result":1}`)
		s.result(r.ID, `"ok"`)
	}()
	var out string
	if err := c.Call(context.Background(), "m", nil, &out); err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestErrorResponseBecomesRPCErrorWithScrubbedText(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() {
		r := s.readReq()
		s.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-5,"message":"limit for +48600100101","data":{"response":{"token":"T"}}},"id":%s}`, r.ID))
	}()
	err := c.Call(context.Background(), "send", nil, nil)
	var re *RPCError
	if !errors.As(err, &re) {
		t.Fatalf("want RPCError, got %v", err)
	}
	if re.Code != -5 || !strings.Contains(re.Message, "+48600100101") || len(re.Data) == 0 {
		t.Fatalf("fields lost: %+v", re)
	}
	if strings.Contains(err.Error(), "48600100101") || !strings.Contains(err.Error(), "-5") {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestEndOfStreamFailsPendingAndLaterCalls(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	res := make(chan error, 1)
	go func() { res <- c.Call(context.Background(), "m", nil, nil) }()
	s.readReq()
	s.outW.Close()
	select {
	case err := <-res:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("want ErrClosed, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call not released")
	}
	<-c.Done()
	if err := c.Call(context.Background(), "m", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("later call: %v", err)
	}
}

func TestCallDoesNotBlockForeverOnAServerThatNeverReads(t *testing.T) {
	c, _ := newPipeClient(t, ClientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "m", map[string]string{"a": strings.Repeat("x", 1<<16)}, nil) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call blocked on the write")
	}
}

func TestUnmarshalableParamsFailBeforeAnythingIsSent(t *testing.T) {
	c, _ := newPipeClient(t, ClientOptions{})
	err := c.Call(context.Background(), "m", map[string]any{"f": func() {}}, nil)
	if err == nil || errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestResultThatDoesNotFitTheTargetIsAnError(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() { r := s.readReq(); s.result(r.ID, `"text"`) }()
	var out struct{ A int }
	if err := c.Call(context.Background(), "m", nil, &out); err == nil {
		t.Fatal("want a decode error")
	}
}

func TestDuplicateAnswerDoesNotStopTheReader(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() {
		r := s.readReq()
		s.result(r.ID, `"one"`)
		s.result(r.ID, `"two"`)
		s.result(r.ID, `"three"`)
		r2 := s.readReq()
		s.result(r2.ID, `"next"`)
	}()
	var out string
	if err := c.Call(context.Background(), "a", nil, &out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, "b", nil, &out); err != nil || out != "next" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
