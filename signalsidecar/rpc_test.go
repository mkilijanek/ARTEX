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
	if err := c.call(context.Background(), "version", nil, &out); err != nil {
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
	if err := c.call(context.Background(), "finishLink", map[string]any{"deviceLinkUri": "u", "deviceName": "n"}, nil); err != nil {
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
			if err := c.call(context.Background(), "listAccounts", map[string]int{"v": i}, &struct {
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
	err := c.call(ctx, "listAccounts", nil, nil)
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
	if err := c.call(context.Background(), "listAccounts", nil, &out); err != nil {
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
	if err := c.call(ctx, "listAccounts", nil, &out); err != nil {
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
	if err := c.call(ctx, "listAccounts", nil, &out); err != nil {
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
	if err := c.call(context.Background(), "listAccounts", nil, &out); err != nil {
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
	if err := c.call(context.Background(), "listAccounts", nil, &out); err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestErrorResponseBecomesRPCErrorWithScrubbedText(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() {
		r := s.readReq()
		s.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-5,"message":"limit for +15555550101","data":{"response":{"token":"T"}}},"id":%s}`, r.ID))
	}()
	err := c.call(context.Background(), "listGroups", nil, nil)
	var re *RPCError
	if !errors.As(err, &re) {
		t.Fatalf("want RPCError, got %v", err)
	}
	if re.Code != -5 || !strings.Contains(re.Message, "+15555550101") || len(re.Data) == 0 {
		t.Fatalf("fields lost: %+v", re)
	}
	if strings.Contains(err.Error(), "15555550101") || !strings.Contains(err.Error(), "-5") {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestEndOfStreamFailsPendingAndLaterCalls(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	res := make(chan error, 1)
	go func() { res <- c.call(context.Background(), "listAccounts", nil, nil) }()
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
	if err := c.call(context.Background(), "listAccounts", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("later call: %v", err)
	}
}

func TestCallDoesNotBlockForeverOnAServerThatNeverReads(t *testing.T) {
	c, _ := newPipeClient(t, ClientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.call(ctx, "listAccounts", map[string]string{"a": strings.Repeat("x", 1<<16)}, nil) }()
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
	err := c.call(context.Background(), "listAccounts", map[string]any{"f": func() {}}, nil)
	if err == nil || errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestResultThatDoesNotFitTheTargetIsAnError(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() { r := s.readReq(); s.result(r.ID, `"text"`) }()
	var out struct{ A int }
	if err := c.call(context.Background(), "listAccounts", nil, &out); err == nil {
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
	if err := c.call(context.Background(), "listAccounts", nil, &out); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.call(ctx, "listAccounts", nil, &out); err != nil || out != "next" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestMethodsOutsideTheAllowlistAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	var mu sync.Mutex
	var written int
	counting := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		written += len(p)
		mu.Unlock()
		return reqW.Write(p)
	})
	c := NewClient(respR, counting, ClientOptions{})
	t.Cleanup(func() { c.Close(); respW.Close(); reqR.Close(); reqW.Close() })
	go io.Copy(io.Discard, reqR)

	methods := []string{"unregister", "removeDevice", "deleteLocalAccountData", "removePin", "setPin", "updateAccount",
		"register", "verify", "listDevices", "sendContacts", "", "Send", "send ", "version\n"}
	for _, m := range methods {
		err := c.call(context.Background(), m, map[string]any{"deleteAccount": true}, nil)
		if !errors.Is(err, ErrMethodNotAllowed) {
			t.Errorf("method %q: got %v", m, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if written != 0 {
		t.Fatalf("%d bytes were written for refused methods", written)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestGroupsAndSendNeedAnAccount(t *testing.T) {
	c, _ := newPipeClient(t, ClientOptions{})
	if _, err := c.ListGroups(context.Background(), ""); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("listGroups: %v", err)
	}
	if _, err := c.Send(context.Background(), "", "g", "m"); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("send: %v", err)
	}
}

func TestTypedCallsUseFixedParameterSets(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	want := map[string]string{
		"version":      `null`,
		"listAccounts": `null`,
		"startLink":    `null`,
		"finishLink":   `{"deviceLinkUri":"sgnl://linkdevice?a=b","deviceName":"dev"}`,
		"listGroups":   `{"account":"+15555550101"}`,
		"send":         `{"account":"+15555550101","groupIds":["gid"],"message":"hi"}`,
	}
	go func() {
		for i := 0; i < len(want); i++ {
			r := s.readReq()
			got := string(r.Params)
			if got == "" {
				got = "null"
			}
			if got != want[r.Method] {
				t.Errorf("%s params %s, want %s", r.Method, got, want[r.Method])
			}
			switch r.Method {
			case "version":
				s.result(r.ID, `{"version":"1"}`)
			case "startLink":
				s.result(r.ID, `{"deviceLinkUri":"u"}`)
			case "finishLink":
				s.result(r.ID, `{"number":"+1","aci":"a"}`)
			case "send":
				s.result(r.ID, `{"timestamp":5,"results":[{"type":"SUCCESS"}]}`)
			default:
				s.result(r.ID, `[]`)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if v, err := c.Version(ctx); err != nil || v != "1" {
		t.Fatalf("version %q %v", v, err)
	}
	if _, err := c.ListAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	if u, err := c.StartLink(ctx); err != nil || u != "u" {
		t.Fatalf("startLink %q %v", u, err)
	}
	if a, err := c.FinishLink(ctx, "sgnl://linkdevice?a=b", "dev"); err != nil || a.Number != "+1" {
		t.Fatalf("finishLink %+v %v", a, err)
	}
	if _, err := c.ListGroups(ctx, "+15555550101"); err != nil {
		t.Fatal(err)
	}
	if r, err := c.Send(ctx, "+15555550101", "gid", "hi"); err != nil || len(r.Results) != 1 || r.Results[0].Type != "SUCCESS" {
		t.Fatalf("send %+v %v", r, err)
	}
}

func TestSendFailureResultsReadsErrorData(t *testing.T) {
	err := &RPCError{Code: -5, Message: "x", Data: json.RawMessage(`{"response":{"results":[{"type":"RATE_LIMIT_FAILURE","token":"T"}]}}`)}
	res, ok := SendFailureResults(fmt.Errorf("wrapped: %w", err))
	if !ok || len(res) != 1 || res[0].Type != "RATE_LIMIT_FAILURE" {
		t.Fatalf("%v %+v", ok, res)
	}
	if _, ok := SendFailureResults(&RPCError{Code: -1, Message: "x"}); ok {
		t.Fatal("no data must not be ok")
	}
	if _, ok := SendFailureResults(errors.New("plain")); ok {
		t.Fatal("plain error must not be ok")
	}
}

func TestSendToNumbersUsesTheRecipientParameterAndNeedsAnAccount(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	if _, err := c.SendNumbers(context.Background(), "", []string{"+15555550102"}, "m"); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("no account: %v", err)
	}
	if _, err := c.SendNumbers(context.Background(), "+15555550101", nil, "m"); !errors.Is(err, ErrNoRecipient) {
		t.Fatalf("no recipient: %v", err)
	}
	got := make(chan reqLine, 1)
	go func() {
		r := s.readReq()
		got <- r
		s.result(r.ID, `{"timestamp":7,"results":[{"type":"SUCCESS"}]}`)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.SendNumbers(ctx, "+15555550101", []string{"+15555550102", "+15555550103"}, "hi")
	if err != nil || res.Timestamp != 7 {
		t.Fatalf("send: %+v %v", res, err)
	}
	r := <-got
	const want = `{"account":"+15555550101","message":"hi","recipient":["+15555550102","+15555550103"]}`
	if r.Method != "send" || string(r.Params) != want {
		t.Fatalf("%s %s, want send %s", r.Method, r.Params, want)
	}
}

func TestSendResultsCarryTheRecipientAddress(t *testing.T) {
	c, s := newPipeClient(t, ClientOptions{})
	go func() {
		r := s.readReq()
		s.result(r.ID, `{"timestamp":7,"results":[{"recipientAddress":{"number":"+15555550102","uuid":"u1"},"type":"SUCCESS"},{"recipientAddress":{"number":null,"uuid":"u2"},"type":"NETWORK_FAILURE"}]}`)
		r = s.readReq()
		s.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-3,"message":"x","data":{"response":{"results":[{"recipientAddress":{"number":"+15555550103"},"type":"UNREGISTERED_FAILURE"}]}}},"id":%s}`, r.ID))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.SendNumbers(ctx, "+15555550101", []string{"+15555550102"}, "hi")
	if err != nil || len(res.Results) != 2 ||
		res.Results[0].Address != (RecipientAddress{Number: "+15555550102", UUID: "u1"}) ||
		res.Results[1].Address != (RecipientAddress{UUID: "u2"}) {
		t.Fatalf("result path: %+v %v", res, err)
	}
	_, err = c.SendNumbers(ctx, "+15555550101", []string{"+15555550103"}, "hi")
	got, ok := SendFailureResults(err)
	if !ok || len(got) != 1 || got[0].Address.Number != "+15555550103" {
		t.Fatalf("error path: %+v %v", got, err)
	}
}
