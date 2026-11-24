// Command fakecli is a scripted stand-in for signal-cli in JSON-RPC mode on
// stdio. It answers the small method subset the sidecar uses, behaves as a
// scenario file says, keeps its accounts in the data directory like the real
// program, and records the calls it received. It exists for tests only.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type scenario struct {
	Version string `json:"version"`
	// LinkURI is what startLink returns.
	LinkURI string `json:"link_uri"`
	// StartLinkError, when set, makes startLink fail with this user error.
	StartLinkError string `json:"start_link_error"`
	Finish         struct {
		// Mode: "after" (complete after AfterMS), "never", "fail", "timeout"
		// (answer "Link request timed out" after AfterMS), "on_stop"
		// (stay pending; the account is kept at the moment the process is
		// stopped, the way a scan that races a cancel ends).
		Mode    string `json:"mode"`
		AfterMS int    `json:"after_ms"`
		Number  string `json:"number"`
		ACI     string `json:"aci"`
		Error   string `json:"error"`
	} `json:"finish"`
	// DelayByMethodMS delays the answer to one method, so that answers leave
	// in another order than the requests arrived.
	DelayByMethodMS map[string]int `json:"delay_by_method_ms"`
	// AnswerDelayMS delays the answers of other requests while a finishLink
	// is pending (the real program answers them a few seconds later).
	AnswerDelayMS int `json:"answer_delay_ms"`
	Send          struct {
		// Mode: "ok", "partial", "allfail", "ratelimit", "ioerror", "unexpected", "untrusted", "unknown_group", "hang".
		Mode string `json:"mode"`
	} `json:"send"`
	Groups []json.RawMessage `json:"groups"`
	// ListGroupsError makes listGroups fail with an I/O error.
	ListGroupsError bool `json:"list_groups_error"`
	// Notifications emits an incoming-message notification with the sentinel
	// body before every answer.
	Notifications  bool   `json:"notifications"`
	NotifySentinel string `json:"notify_sentinel"`
	// Oversize emits one line of Bytes bytes before answering Method.
	Oversize struct {
		Method string `json:"method"`
		Bytes  int    `json:"bytes"`
	} `json:"oversize"`
	// CrashOn exits with status 3 on receiving this method, without answering.
	CrashOn string `json:"crash_on"`
	// HangOn never answers this method.
	HangOn string `json:"hang_on"`
	// HangVersionAfterMS stops answering version this long after start (0:
	// always answer), the way a JVM that is up but stuck does.
	HangVersionAfterMS int `json:"hang_version_after_ms"`
	// ExitAfterStartMS exits with status 4 this long after start (0: never).
	ExitAfterStartMS int `json:"exit_after_start_ms"`
	// StartDelayMS waits before reading the first request.
	StartDelayMS int `json:"start_delay_ms"`
	// CallsFile receives one JSON line per request.
	CallsFile string `json:"calls_file"`
	// StartsFile receives one line per process start: the pid and the time.
	StartsFile string `json:"starts_file"`
	// DumpFile receives the arguments and the environment at start.
	DumpFile string `json:"dump_file"`
	// StderrLines are written to standard error at start.
	StderrLines []string `json:"stderr_lines"`
}

type account struct {
	Number string `json:"number"`
	ACI    string `json:"aci"`
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type server struct {
	sc      scenario
	dataDir string
	args    []string

	started time.Time
	out     sync.Mutex
	w       io.Writer

	mu      sync.Mutex
	pending int // finishLink requests in flight
	issued  map[string]bool
}

func main() {
	var scPath, dataDir string
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--scenario":
			if i+1 < len(args) {
				scPath = args[i+1]
				i++
			}
		case "-c":
			if i+1 < len(args) {
				dataDir = args[i+1]
				i++
			}
		}
	}
	s := &server{dataDir: dataDir, args: args, w: os.Stdout, issued: map[string]bool{}}
	// An image that fixes the command line has no scenario flag: take the
	// scenario from a file in the data directory when there is one. Only this
	// stand-in reads it; the real signal-cli has no such file.
	if scPath == "" && dataDir != "" {
		if _, err := os.Stat(filepath.Join(dataDir, "fake-scenario.json")); err == nil {
			scPath = filepath.Join(dataDir, "fake-scenario.json")
		}
	}
	if scPath != "" {
		raw, err := os.ReadFile(scPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakecli: scenario:", err)
			os.Exit(2)
		}
		if err := json.Unmarshal(raw, &s.sc); err != nil {
			fmt.Fprintln(os.Stderr, "fakecli: scenario:", err)
			os.Exit(2)
		}
	}
	if s.sc.Version == "" {
		s.sc.Version = "0.14.10"
	}
	if s.sc.LinkURI == "" {
		s.sc.LinkURI = "sgnl://linkdevice?uuid=ZmFrZS11dWlkLWZvci10ZXN0cw&pub_key=BQabcdefghijklmnopqrstuvwxyz0123456789%2BABCDEF&capabilities=a,b"
	}
	s.started = time.Now()
	if s.sc.StartsFile != "" {
		appendLine(s.sc.StartsFile, fmt.Sprintf("%d %d", os.Getpid(), time.Now().UnixNano()))
	}
	if s.sc.DumpFile != "" {
		raw, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Environ()})
		_ = os.WriteFile(s.sc.DumpFile, raw, 0o600)
	}
	for _, l := range s.sc.StderrLines {
		fmt.Fprintln(os.Stderr, l)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigs
		s.stop()
	}()
	if s.sc.ExitAfterStartMS > 0 {
		go func() {
			time.Sleep(time.Duration(s.sc.ExitAfterStartMS) * time.Millisecond)
			os.Exit(4)
		}()
	}
	if s.sc.StartDelayMS > 0 {
		time.Sleep(time.Duration(s.sc.StartDelayMS) * time.Millisecond)
	}

	rd := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := rd.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var req request
			if jerr := json.Unmarshal(line, &req); jerr != nil {
				s.write(map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": -32700, "message": "Parse error"}, "id": nil})
			} else {
				if s.sc.CallsFile != "" {
					rec, _ := json.Marshal(map[string]any{"method": req.Method, "params": req.Params})
					appendLine(s.sc.CallsFile, string(rec))
				}
				if req.Method == s.sc.CrashOn {
					os.Exit(3)
				}
				go s.handle(req)
			}
		}
		if err != nil {
			s.stop()
			return
		}
	}
}

// stop runs on SIGTERM and on end of input: a pending link that is scripted to
// complete at that moment keeps its account, then the process exits.
func (s *server) stop() {
	s.mu.Lock()
	pending := s.pending
	s.mu.Unlock()
	if pending > 0 && s.sc.Finish.Mode == "on_stop" {
		s.addAccount()
	}
	os.Exit(0)
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

func (s *server) write(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.out.Lock()
	defer s.out.Unlock()
	_, _ = s.w.Write(append(raw, '\n'))
}

func (s *server) writeRaw(b []byte) {
	s.out.Lock()
	defer s.out.Unlock()
	_, _ = s.w.Write(b)
}

func (s *server) reply(id json.RawMessage, result any) {
	s.write(map[string]any{"jsonrpc": "2.0", "result": result, "id": id})
}

func (s *server) fail(id json.RawMessage, code int, msg string, data any) {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	s.write(map[string]any{"jsonrpc": "2.0", "error": e, "id": id})
}

func (s *server) accountsPath() string { return s.dataDir + "/accounts.json" }

func (s *server) accounts() []account {
	if s.dataDir == "" {
		return []account{}
	}
	raw, err := os.ReadFile(s.accountsPath())
	if err != nil {
		return []account{}
	}
	var out []account
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return []account{}
	}
	return out
}

func (s *server) addAccount() {
	if s.dataDir == "" {
		return
	}
	acc := s.accounts()
	n := s.sc.Finish.Number
	if n == "" {
		n = "+48600100101"
	}
	a := s.sc.Finish.ACI
	if a == "" {
		a = "11111111-2222-4333-8444-555555555555"
	}
	acc = append(acc, account{Number: n, ACI: a})
	raw, _ := json.Marshal(acc)
	_ = os.WriteFile(s.accountsPath(), raw, 0o600)
}

func (s *server) handle(req request) {
	isFinish := req.Method == "finishLink"
	if isFinish {
		s.mu.Lock()
		s.pending++
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
		}()
	} else if s.sc.AnswerDelayMS > 0 {
		s.mu.Lock()
		p := s.pending
		s.mu.Unlock()
		if p > 0 {
			time.Sleep(time.Duration(s.sc.AnswerDelayMS) * time.Millisecond)
		}
	}
	if d := s.sc.DelayByMethodMS[req.Method]; d > 0 {
		time.Sleep(time.Duration(d) * time.Millisecond)
	}
	if s.sc.Notifications {
		s.write(map[string]any{"jsonrpc": "2.0", "method": "receive", "params": map[string]any{
			"envelope": map[string]any{"source": "+48600100999", "dataMessage": map[string]any{"message": s.sc.NotifySentinel}},
		}})
	}
	if s.sc.Oversize.Bytes > 0 && s.sc.Oversize.Method == req.Method {
		pad := make([]byte, s.sc.Oversize.Bytes)
		for i := range pad {
			pad[i] = 'x'
		}
		head := `{"jsonrpc":"2.0","method":"receive","params":{"pad":"`
		line := append([]byte(head), pad...)
		line = append(line, '"', '}', '}', '\n')
		s.writeRaw(line)
	}
	if req.Method == s.sc.HangOn {
		select {}
	}
	if req.Method == "version" && s.sc.HangVersionAfterMS > 0 &&
		time.Since(s.started) > time.Duration(s.sc.HangVersionAfterMS)*time.Millisecond {
		select {}
	}
	switch req.Method {
	case "version":
		s.reply(req.ID, map[string]any{"version": s.sc.Version})
	case "listAccounts":
		out := []map[string]any{}
		for _, a := range s.accounts() {
			out = append(out, map[string]any{"number": a.Number, "aci": a.ACI})
		}
		s.reply(req.ID, out)
	case "startLink":
		if s.sc.StartLinkError != "" {
			s.fail(req.ID, -3, s.sc.StartLinkError, nil)
			return
		}
		s.mu.Lock()
		s.issued[s.sc.LinkURI] = true
		s.mu.Unlock()
		s.reply(req.ID, map[string]any{"deviceLinkUri": s.sc.LinkURI})
	case "finishLink":
		s.finishLink(req)
	case "listGroups":
		if !s.checkAccount(req) {
			return
		}
		if s.sc.ListGroupsError {
			s.fail(req.ID, -3, "Failed to list groups (IOException)", nil)
			return
		}
		groups := s.sc.Groups
		if groups == nil {
			groups = []json.RawMessage{}
		}
		s.reply(req.ID, groups)
	case "send":
		s.send(req)
	default:
		s.fail(req.ID, -32601, "Method not implemented", nil)
	}
}

// checkAccount mirrors multi-account mode: a call must name a loaded account.
func (s *server) checkAccount(req request) bool {
	var p struct {
		Account string `json:"account"`
	}
	_ = json.Unmarshal(req.Params, &p)
	for _, a := range s.accounts() {
		if p.Account != "" && a.Number == p.Account {
			return true
		}
	}
	s.fail(req.ID, -32602, "Specified account does not exist", nil)
	return false
}

func (s *server) finishLink(req request) {
	var p struct {
		URI string `json:"deviceLinkUri"`
	}
	_ = json.Unmarshal(req.Params, &p)
	s.mu.Lock()
	known := s.issued[p.URI]
	delete(s.issued, p.URI) // a link URI is good for one finishLink
	s.mu.Unlock()
	if !known {
		s.fail(req.ID, -1, "Unknown device link uri.", nil)
		return
	}
	switch s.sc.Finish.Mode {
	case "after":
		time.Sleep(time.Duration(s.sc.Finish.AfterMS) * time.Millisecond)
		s.addAccount()
		n := s.sc.Finish.Number
		if n == "" {
			n = "+48600100101"
		}
		a := s.sc.Finish.ACI
		if a == "" {
			a = "11111111-2222-4333-8444-555555555555"
		}
		s.reply(req.ID, map[string]any{"number": n, "aci": a})
	case "timeout":
		time.Sleep(time.Duration(s.sc.Finish.AfterMS) * time.Millisecond)
		s.fail(req.ID, -1, "Link request timed out, please try again.", nil)
	case "fail":
		time.Sleep(time.Duration(s.sc.Finish.AfterMS) * time.Millisecond)
		msg := s.sc.Finish.Error
		if msg == "" {
			msg = "Link request error: boom (IOException)"
		}
		s.fail(req.ID, -3, msg, nil)
	default: // "never", "on_stop", empty
		select {}
	}
}

func (s *server) send(req request) {
	if !s.checkAccount(req) {
		return
	}
	recipient := map[string]any{"number": nil, "uuid": "22222222-3333-4444-8555-666666666666"}
	switch s.sc.Send.Mode {
	case "partial":
		s.reply(req.ID, map[string]any{"timestamp": 1700000000000, "results": []map[string]any{
			{"recipientAddress": recipient, "type": "SUCCESS"},
			{"recipientAddress": recipient, "type": "NETWORK_FAILURE"},
		}})
	case "allfail":
		s.fail(req.ID, -3, "Failed to send message", map[string]any{"response": map[string]any{
			"timestamp": 1700000000000,
			"results":   []map[string]any{{"recipientAddress": recipient, "type": "NETWORK_FAILURE"}},
		}})
	case "ratelimit":
		s.fail(req.ID, -5, "Failed to send message due to rate limiting", map[string]any{"response": map[string]any{
			"timestamp": 1700000000000,
			"results":   []map[string]any{{"recipientAddress": recipient, "type": "RATE_LIMIT_FAILURE", "token": "CHALLENGE-SENTINEL", "retryAfterSeconds": 60}},
		}})
	case "ioerror":
		s.fail(req.ID, -3, "Failed to send message: connection reset (IOException)", nil)
	case "unexpected":
		s.fail(req.ID, -32603, "Failed to send message: connection reset (SocketException)", nil)
	case "untrusted":
		s.fail(req.ID, -4, "Failed to send message: untrusted identity", nil)
	case "unknown_group":
		s.fail(req.ID, -1, "Failed to send to group: Unknown group", nil)
	case "hang":
		select {}
	default:
		s.reply(req.ID, map[string]any{"timestamp": 1700000000000, "results": []map[string]any{
			{"recipientAddress": recipient, "type": "SUCCESS"},
		}})
	}
}
