package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/sandbox"
)

// The file tools of the SDK (Read, Write, Edit, MultiEdit, LS, Glob, Grep) take any path the model
// names and, run inside the server process, use the server's own rights. With isolation on they
// run in a subprocess instead: `artex __toolfs`, started through the drop chain, so every open,
// write and directory walk happens as the tool user. The request goes in as JSON on stdin, the
// result comes back as JSON on stdout. A failing helper is a tool error; there is no fallback to
// the in-process tool.

// fileToolNames are the tools that run out of process.
var fileToolNames = []string{"Read", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep"}

func isFileTool(name string) bool {
	for _, n := range fileToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// newFileTool builds the SDK tool of that name, or nil.
func newFileTool(name string) actool.CoreTool {
	switch name {
	case "Read":
		return actool.NewRead()
	case "Write":
		return actool.NewWrite()
	case "Edit":
		return actool.NewEdit()
	case "MultiEdit":
		return actool.NewMultiEdit()
	case "LS":
		return actool.NewLS()
	case "Glob":
		return actool.NewGlob()
	case "Grep":
		return actool.NewGrep()
	}
	return nil
}

// Limits of one helper call. They are variables so the tests can shorten them.
var (
	toolfsTimeout     = 120 * time.Second
	toolfsMaxOutput   = 16 << 20 // bytes the helper may answer with
	toolfsMaxRequest  = 64 << 20 // bytes of request the helper reads
	toolfsMaxFileSize = int64(256 << 20)
)

// liftedMaxOutput is the output budget the helper gives tools that cut their own output. The helper
// cannot spill (the spill directory is the server's, and writing it is the server's job), and a
// cut made in the helper would throw away what the server would have kept in a file. So the
// helper returns such a tool's whole output and the server applies the real budget.
const liftedMaxOutput = 1 << 30

// toolfsRequest is what the server writes to the helper's stdin.
type toolfsRequest struct {
	Tool string `json:"tool"`
	// Input is the model's call arguments, byte for byte (an empty input stays empty, so the
	// tool's own error for it is the same as in process).
	Input          []byte `json:"input"`
	WorkingDir     string `json:"working_dir"`
	MaxOutputChars int    `json:"max_output_chars"`
	// Debug makes the answer name the ripgrep binary in use. The server never sets it; the
	// private test harness sends it to a helper it starts itself.
	Debug bool `json:"debug,omitempty"`
}

// toolfsResponse is what the helper writes to stdout. Only text comes back: the file tools return
// nothing else, and anything else is refused rather than passed on to the model.
type toolfsResponse struct {
	Texts   []string `json:"texts,omitempty"`
	IsError bool     `json:"is_error,omitempty"`
	// Error carries a Go error of the tool itself (for example, input that is not JSON). The server
	// turns it into the same error the in-process tool would have returned.
	Error string  `json:"error,omitempty"`
	Rg    *string `json:"rg,omitempty"`
}

// RunToolfs is the helper: it reads one request, runs the SDK tool, and writes the response. It runs
// as the tool user, started by the `__toolfs` stage of the artex binary.
func RunToolfs(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(in, int64(toolfsMaxRequest)+1))
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	if len(raw) > toolfsMaxRequest {
		return fmt.Errorf("request larger than %d bytes", toolfsMaxRequest)
	}
	var req toolfsRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	t := newFileTool(req.Tool)
	if t == nil {
		return fmt.Errorf("%q is not a file tool", req.Tool)
	}
	// The helper starts fresh for every call, so a missing ripgrep would be looked for with npm
	// every time. The image carries it; without it the pure-Go search answers.
	if err := os.Setenv("NORMA_RIPGREP_NO_INSTALL", "1"); err != nil {
		return err
	}

	resp := toolfsRun(t, req)
	if req.Debug {
		rg := actool.RipgrepPath()
		resp.Rg = &rg
	}
	size := 0
	for _, s := range resp.Texts {
		size += len(s)
	}
	if size > toolfsMaxOutput {
		resp = toolfsResponse{Error: fmt.Sprintf("the result is larger than %d bytes", toolfsMaxOutput)}
	}
	return json.NewEncoder(out).Encode(resp)
}

func toolfsRun(t actool.CoreTool, req toolfsRequest) toolfsResponse {
	if msg := checkFileSize(req); msg != "" {
		return toolfsResponse{Texts: []string{msg}, IsError: true}
	}
	max := req.MaxOutputChars
	// Read pages its own output against the budget; the others cut theirs (see liftedMaxOutput).
	if req.Tool != "Read" {
		max = liftedMaxOutput
	}
	tc := &actool.ToolContext{WorkingDir: req.WorkingDir, MaxOutputChars: max}
	res, err := t.Call(context.Background(), req.Input, tc)
	if err != nil {
		return toolfsResponse{Error: err.Error()}
	}
	if len(res.Extra) != 0 {
		return toolfsResponse{Error: "the tool returned messages the helper does not carry"}
	}
	resp := toolfsResponse{IsError: res.IsError}
	for _, b := range res.Content {
		if b.Type != llm.BlockText {
			return toolfsResponse{Error: "the tool returned a block that is not text"}
		}
		resp.Texts = append(resp.Texts, b.Text)
	}
	return resp
}

// checkFileSize keeps a tool that reads a whole file into memory from reading a device that never
// ends or a file larger than the helper should hold. It answers with the text of a tool error, or
// "" when the call may go on (including when the path cannot be examined: the tool says why).
func checkFileSize(req toolfsRequest) string {
	switch req.Tool {
	case "Read", "Edit", "MultiEdit":
	default:
		return ""
	}
	var in struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(req.Input, &in) != nil || in.FilePath == "" {
		return ""
	}
	path := in.FilePath
	if !filepath.IsAbs(path) && req.WorkingDir != "" {
		path = filepath.Join(req.WorkingDir, path)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return ""
	}
	switch {
	case !fi.Mode().IsRegular():
		return fmt.Sprintf("Error reading %s: not a regular file", in.FilePath)
	case fi.Size() > toolfsMaxFileSize:
		return fmt.Sprintf("Error reading %s: the file is larger than %d bytes", in.FilePath, toolfsMaxFileSize)
	}
	return ""
}

// isolatedFileTool is a file tool whose Call runs in the helper when isolation is on. Everything
// else (name, description, schema, flags, permission check) is the SDK tool's, so what the model
// sees does not change. The wrapped tool is the SDK's own, or a decorator chain around it: the
// wrapper must be the innermost layer, so metering, call counting and schema defaults keep
// running above it (see defaultTools).
type isolatedFileTool struct {
	actool.CoreTool
}

// AcceptsRawInput keeps the SDK's optional repair-tool contract through the wrapper.
func (t *isolatedFileTool) AcceptsRawInput() bool {
	r, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && r.AcceptsRawInput()
}

// Call runs the tool in the helper. With isolation off the SDK tool runs in process, as before.
func (t *isolatedFileTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	iso := sandbox.Current()
	if !iso.On() {
		return t.CoreTool.Call(ctx, input, tc)
	}
	if tc == nil {
		tc = &actool.ToolContext{}
	}
	// A call holds a lease of its own: the helper is a tool-user process. A guarded session
	// holds one too, so this only matters for a tool called outside a session.
	lease, err := iso.Leases().Acquire(ctx)
	if err != nil {
		return actool.Result{}, fmt.Errorf("take a tool-process lease: %w", err)
	}
	defer lease.Release()

	name := t.Name()
	dir := tc.WorkingDir
	if dir != "" {
		if dir, err = filepath.Abs(dir); err != nil {
			return actool.Result{}, fmt.Errorf("working directory: %w", err)
		}
	}
	req := toolfsRequest{Tool: name, Input: input, WorkingDir: dir, MaxOutputChars: tc.MaxOutputChars}
	resp, err := callToolfs(ctx, iso, lease, req)
	if err != nil {
		log.Printf("[toolfs] %s: %v", name, err)
		return actool.Result{}, fmt.Errorf("%s could not run: %w", name, err)
	}
	if resp.Error != "" {
		return actool.Result{}, errors.New(resp.Error)
	}
	res := actool.Result{IsError: resp.IsError}
	for _, s := range resp.Texts {
		// The tools that cut their own output do it here, against the session's spill directory
		// and the harness's budget, exactly as the SDK tool would have done in process.
		if !resp.IsError && (name == "LS" || name == "Glob" || name == "Grep") {
			s = actool.Capture(tc, s)
		}
		res.Content = append(res.Content, llm.TextBlock(s))
	}
	return res, nil
}

// callToolfs starts the helper through the drop chain, feeds it the request and reads the answer.
// The call is bounded in time and output, and the whole process group is killed on timeout.
func callToolfs(ctx context.Context, iso *sandbox.Isolation, lease *sandbox.Lease, req toolfsRequest) (toolfsResponse, error) {
	var resp toolfsResponse
	body, err := json.Marshal(req)
	if err != nil {
		return resp, fmt.Errorf("encode request: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, toolfsTimeout)
	defer cancel()
	cmd, cleanup, err := iso.Command(cctx, lease, sandbox.Spec{
		Dir: req.WorkingDir, Program: iso.Layout().Artex, Args: []string{sandbox.CmdToolfs},
	})
	if err != nil {
		return resp, fmt.Errorf("prepare the helper: %w", err)
	}
	defer cleanup()
	isolateProcess(cmd)
	stdout := &capBuffer{max: toolfsMaxOutput, onOver: cancel}
	stderr := &capBuffer{max: 4096, drop: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(body), stdout, stderr

	runErr := cmd.Run()
	switch {
	case stdout.over:
		return resp, fmt.Errorf("the helper's output is larger than %d bytes", toolfsMaxOutput)
	case errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return resp, fmt.Errorf("the helper did not answer within %s", toolfsTimeout)
	case ctx.Err() != nil:
		return resp, fmt.Errorf("the call was cancelled: %w", ctx.Err())
	case runErr != nil:
		msg := firstLineOf(stderr.String())
		if msg != "" {
			return resp, fmt.Errorf("the helper failed: %w (%s)", runErr, msg)
		}
		return resp, fmt.Errorf("the helper failed: %w", runErr)
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return toolfsResponse{}, fmt.Errorf("the helper's answer is not valid: %w", err)
	}
	return resp, nil
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// capBuffer collects at most max bytes. Past that it either drops the rest (drop) or marks the
// overflow and calls onOver, which ends the process: a reader that stops reading would otherwise
// leave the child blocked on a full pipe.
type capBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	max    int
	drop   bool
	over   bool
	onOver func()
}

func (b *capBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len()+len(p) > b.max {
		if b.drop {
			if room := b.max - b.buf.Len(); room > 0 {
				b.buf.Write(p[:room])
			}
			return len(p), nil
		}
		if !b.over {
			b.over = true
			if b.onOver != nil {
				b.onOver()
			}
		}
		return len(p), nil // keep draining; the process is being killed
	}
	return b.buf.Write(p)
}

func (b *capBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Bytes()
}

func (b *capBuffer) String() string { return string(b.Bytes()) }

// defaultTools is actool.DefaultTools() with the file tools made ready to run out of process. It
// is the list every agent kind starts from. The wrapper is the innermost layer, below the
// decorators (call counting, usage metering, schema defaults) that AugmentTools adds, so those keep
// running; the session guard checks that it is still there (isolateSessionTools).
func defaultTools() []actool.CoreTool {
	tools := actool.DefaultTools()
	for i, t := range tools {
		if isFileTool(t.Name()) {
			tools[i] = &isolatedFileTool{CoreTool: t}
		}
	}
	return tools
}

// innermostTool follows embedded CoreTool fields (the shape of every decorator in the program)
// down to the tool that does the work. A wrapper type that hides its inner tool ends the walk at
// itself.
func innermostTool(t actool.CoreTool) actool.CoreTool {
	for depth := 0; depth < 32; depth++ {
		if _, ok := t.(*isolatedFileTool); ok {
			return t
		}
		v := reflect.ValueOf(t)
		if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
			return t
		}
		f := v.Elem().FieldByName("CoreTool")
		if !f.IsValid() || f.Kind() != reflect.Interface || f.IsNil() || !f.CanInterface() {
			return t
		}
		inner, ok := f.Interface().(actool.CoreTool)
		if !ok {
			return t
		}
		t = inner
	}
	return t
}

// isolateSessionTools is the guard's part: whatever the list, every file tool in it runs out of
// process when isolation is on. A file tool whose innermost layer is already the wrapper is kept
// as it is. Any other (a bare SDK tool, or a decorator the walk cannot see through) is wrapped from
// the outside: it is safe, but the decorators of that chain no longer run, so it is logged.
func isolateSessionTools(tools []actool.CoreTool) []actool.CoreTool {
	var out []actool.CoreTool
	for i, t := range tools {
		if !isFileTool(t.Name()) {
			continue
		}
		if _, ok := innermostTool(t).(*isolatedFileTool); ok {
			continue
		}
		if out == nil {
			out = append([]actool.CoreTool(nil), tools...)
		}
		log.Printf("[isolation] file tool %s was not built through defaultTools; it is wrapped from the outside, so any decorators around it do not run", t.Name())
		out[i] = &isolatedFileTool{CoreTool: t}
	}
	if out == nil {
		return tools
	}
	return out
}
