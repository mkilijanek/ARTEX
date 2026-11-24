package signalsidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// DefaultMaxLine is the longest line the reader keeps. A longer line is
// discarded to its end and reading goes on.
const DefaultMaxLine = 1 << 20

// ErrClosed is returned by every call that cannot be completed because the
// stream to signal-cli ended or the client was closed.
var ErrClosed = errors.New("signal-cli connection closed")

// RPCError is an error object of a JSON-RPC response. Message and Data are as
// received and may hold numbers or group ids: Error() masks them, so use the
// fields only where the content is handled on purpose.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("signal-cli error %d: %s", e.Code, Scrub(e.Message))
}

// ClientOptions tune a Client; the zero value is usable.
type ClientOptions struct {
	// MaxLine is the longest accepted line in bytes (default DefaultMaxLine).
	MaxLine int
	// Logf receives one short line per skipped oversize line. It never gets
	// content.
	Logf func(format string, args ...any)
}

// Client is a JSON-RPC 2.0 client over a pair of streams (the stdin and stdout
// of signal-cli). Calls may run concurrently; answers are matched by id.
// Incoming requests and notifications, which signal-cli uses for received
// messages, are counted and dropped without being looked at.
type Client struct {
	maxLine int
	logf    func(string, ...any)

	out chan []byte // frames for the writer goroutine

	mu      sync.Mutex
	pending map[uint64]chan response
	closed  bool
	err     error
	done    chan struct{}

	nextID        atomic.Uint64
	skipped       atomic.Uint64
	notifications atomic.Uint64
}

type response struct {
	result json.RawMessage
	err    *RPCError
}

// NewClient starts the reader and writer goroutines. They end when r reports
// an error or end of stream, when a write fails, or after Close. Close does
// not close r or w: the owner of the streams does, which also releases a
// writer that is blocked in a write.
func NewClient(r io.Reader, w io.Writer, opts ClientOptions) *Client {
	c := &Client{
		maxLine: opts.MaxLine,
		logf:    opts.Logf,
		out:     make(chan []byte),
		pending: make(map[uint64]chan response),
		done:    make(chan struct{}),
	}
	if c.maxLine <= 0 {
		c.maxLine = DefaultMaxLine
	}
	go c.readLoop(r)
	go c.writeLoop(w)
	return c
}

// Done is closed when the client can make no more calls.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close fails every pending and later call with ErrClosed.
func (c *Client) Close() { c.closeWith(nil) }

// Skipped is the number of oversize lines discarded so far.
func (c *Client) Skipped() uint64 { return c.skipped.Load() }

// Notifications is the number of incoming requests and notifications dropped.
func (c *Client) Notifications() uint64 { return c.notifications.Load() }

// Pending is the number of calls waiting for an answer.
func (c *Client) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func (c *Client) closeWith(cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if cause != nil {
		c.err = fmt.Errorf("%w: %v", ErrClosed, cause)
	} else {
		c.err = ErrClosed
	}
	close(c.done)
}

func (c *Client) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return ErrClosed
}

// allowedMethods is every method the client will put on the wire. Anything
// else is refused before a byte is written: methods such as unregister with
// deleteAccount would delete the operator's Signal account.
var allowedMethods = map[string]bool{
	"version":      true,
	"listAccounts": true,
	"startLink":    true,
	"finishLink":   true,
	"listGroups":   true,
	"send":         true,
}

// ErrMethodNotAllowed is returned for a method outside the allowlist.
var ErrMethodNotAllowed = errors.New("signal-cli method not allowed")

// ErrNoAccount is returned for a listGroups or send without an account: in
// multi-account mode signal-cli would silently pick the single loaded one.
var ErrNoAccount = errors.New("signal-cli call without an account")

// call sends one request and waits for its answer, for the context to end, or
// for the client to close. The deadline of ctx is the timeout of this request
// only. A nil result discards the result value.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	if !allowedMethods[method] {
		return fmt.Errorf("%w: %q", ErrMethodNotAllowed, method)
	}
	frame, id, err := c.frame(method, params)
	if err != nil {
		return err
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.closedErr()
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	select {
	case c.out <- frame:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.closedErr()
	}
	select {
	case r := <-ch:
		return decodeResponse(method, r, result)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		select { // an answer that arrived together with the end of stream still counts
		case r := <-ch:
			return decodeResponse(method, r, result)
		default:
			return c.closedErr()
		}
	}
}

func decodeResponse(method string, r response, result any) error {
	if r.err != nil {
		return r.err
	}
	if result == nil || len(r.result) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.result, result); err != nil {
		return fmt.Errorf("signal-cli %s: result does not fit: %w", method, err)
	}
	return nil
}

func (c *Client) frame(method string, params any) ([]byte, uint64, error) {
	id := c.nextID.Add(1)
	b, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
		ID      uint64 `json:"id"`
	}{"2.0", method, params, id})
	if err != nil {
		return nil, 0, fmt.Errorf("signal-cli %s: encode request: %w", method, err)
	}
	return append(b, '\n'), id, nil
}

func (c *Client) writeLoop(w io.Writer) {
	for {
		select {
		case frame := <-c.out:
			if _, err := w.Write(frame); err != nil {
				c.closeWith(fmt.Errorf("write: %w", err))
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Client) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	for {
		line, over, err := readBoundedLine(br, buf[:0], c.maxLine)
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.closeWith(errors.New("end of output"))
			} else {
				c.closeWith(fmt.Errorf("read: %w", err))
			}
			return
		}
		buf = line
		if over {
			n := c.skipped.Add(1)
			if c.logf != nil {
				c.logf("[signal] skipped an oversize line from signal-cli (%d so far)", n)
			}
			continue
		}
		c.dispatch(line)
	}
}

// readBoundedLine reads one line (without its newline), keeping at most max
// bytes. A longer line is read to its end and reported as over with no data,
// so memory stays bounded and the next call starts at the next line. A final
// line without a newline is dropped: the writer died in the middle of it.
func readBoundedLine(br *bufio.Reader, buf []byte, max int) (line []byte, over bool, err error) {
	for {
		chunk, rerr := br.ReadSlice('\n')
		full := errors.Is(rerr, bufio.ErrBufferFull)
		if rerr != nil && !full {
			return nil, false, rerr
		}
		if !over {
			data := chunk
			if !full {
				data = data[:len(data)-1]
			}
			if len(buf)+len(data) > max {
				over = true
				buf = buf[:0]
			} else {
				buf = append(buf, data...)
			}
		}
		if !full {
			if !over && len(buf) > 0 && buf[len(buf)-1] == '\r' {
				buf = buf[:len(buf)-1]
			}
			return buf, over, nil
		}
	}
}

func (c *Client) dispatch(line []byte) {
	if len(line) == 0 {
		return
	}
	var m struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return // not a JSON object: nothing to match it to
	}
	if m.Method != "" {
		c.notifications.Add(1)
		return
	}
	id, err := strconv.ParseUint(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()
	if !ok {
		return // answer to a call that gave up
	}
	select {
	case ch <- response{result: m.Result, err: m.Error}:
	default: // a second answer for the same id
	}
}

// Account is an account of the signal-cli data directory.
type Account struct {
	Number string `json:"number"`
	ACI    string `json:"aci"`
}

// Group is one entry of listGroups. The list also holds groups the account
// has left and blocked groups; the caller filters.
type Group struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsMember  bool   `json:"isMember"`
	IsBlocked bool   `json:"isBlocked"`
}

// SendRecipientResult is the outcome for one recipient of a send. Proof
// tokens and retry hints are not decoded.
type SendRecipientResult struct {
	Type    string `json:"type"`
	GroupID string `json:"groupId,omitempty"`
}

// SendResult is the result of a send with at least one delivered recipient.
type SendResult struct {
	Timestamp int64                 `json:"timestamp"`
	Results   []SendRecipientResult `json:"results"`
}

// SendFailureResults extracts the per-recipient results that signal-cli puts
// in error.data.response when no recipient succeeded. ok is false when err is
// not an RPCError that carries them.
func SendFailureResults(err error) (results []SendRecipientResult, ok bool) {
	var re *RPCError
	if !errors.As(err, &re) || len(re.Data) == 0 {
		return nil, false
	}
	var d struct {
		Response struct {
			Results []SendRecipientResult `json:"results"`
		} `json:"response"`
	}
	if json.Unmarshal(re.Data, &d) != nil || d.Response.Results == nil {
		return nil, false
	}
	return d.Response.Results, true
}

// Version asks signal-cli for its version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.call(ctx, "version", nil, &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

// ListAccounts lists the accounts known to signal-cli.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	var out []Account
	if err := c.call(ctx, "listAccounts", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StartLink begins a device link and returns the link URI.
func (c *Client) StartLink(ctx context.Context) (string, error) {
	var out struct {
		URI string `json:"deviceLinkUri"`
	}
	if err := c.call(ctx, "startLink", nil, &out); err != nil {
		return "", err
	}
	return out.URI, nil
}

// FinishLink waits until the phone has scanned the URI. It answers when the
// link is done, fails, or signal-cli gives up (about 120 s); ctx can end the
// wait earlier, but only a restart of signal-cli aborts the link itself.
func (c *Client) FinishLink(ctx context.Context, uri, deviceName string) (Account, error) {
	var out Account
	err := c.call(ctx, "finishLink", map[string]string{"deviceLinkUri": uri, "deviceName": deviceName}, &out)
	return out, err
}

// ListGroups lists the groups of the account.
func (c *Client) ListGroups(ctx context.Context, account string) ([]Group, error) {
	if account == "" {
		return nil, ErrNoAccount
	}
	var out []Group
	if err := c.call(ctx, "listGroups", map[string]string{"account": account}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Send sends a text message to one group (the id in signal-cli's own form).
// When no recipient is reached signal-cli answers with an error; see
// SendFailureResults.
func (c *Client) Send(ctx context.Context, account, groupID, message string) (SendResult, error) {
	if account == "" {
		return SendResult{}, ErrNoAccount
	}
	var out SendResult
	err := c.call(ctx, "send", map[string]any{
		"account":  account,
		"groupIds": []string{groupID},
		"message":  message,
	}, &out)
	return out, err
}
