package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Exit codes of the stages. A stage never prints a value; the codes and the messages name only
// keys, paths and ids.
const (
	ExitDeniedVar = 97  // a per-call variable was refused
	ExitCannotRun = 126 // the next stage could not be started
	ExitNotFound  = 127 // the program was not found
	ExitUsage     = 64
)

// StageError is a failure with the exit code the stage ends with.
type StageError struct {
	Code int
	Msg  string
	Err  error
}

func (e *StageError) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}
func (e *StageError) Unwrap() error { return e.Err }

// ExitCodeOf is the exit code a stage ends with for err.
func ExitCodeOf(err error) int {
	var se *StageError
	if errors.As(err, &se) {
		return se.Code
	}
	return 1
}

func usage(format string, a ...any) error {
	return &StageError{Code: ExitUsage, Msg: fmt.Sprintf(format, a...)}
}

// StageArgs is the command line of `__astool` and `__toolenv`:
//
//	__astool  [--cd DIR] [--vars FILE]   -- PROGRAM ARGS...
//	__toolenv [--cd DIR] [--vars-fd N]   -- PROGRAM ARGS...
//
// The root stage names the variables file; the second stage is told the descriptor that carries
// it. The descriptor number is explicit because a Go process keeps descriptors of its own open
// from the start (the runtime holds a cgroup file on hosts that have one, and that file can be
// descriptor 3), so "descriptor 3 if there is one" cannot tell variables from the runtime's file.
type StageArgs struct {
	CD      string
	Vars    string
	VarsFD  int
	Program string
	Args    []string
}

// ParseStageArgs reads the flags before the first `--`; everything after it is the program and
// its arguments and is never interpreted, whatever it looks like. root selects the flag set:
// the root stage takes --vars, the second stage --vars-fd.
func ParseStageArgs(args []string, root bool) (StageArgs, error) {
	allowVars := root
	var a StageArgs
	i := 0
	for ; i < len(args); i++ {
		switch args[i] {
		case "--":
			rest := args[i+1:]
			if len(rest) == 0 || rest[0] == "" {
				return a, usage("no program after --")
			}
			a.Program, a.Args = rest[0], rest[1:]
			return a, nil
		case "--cd":
			if a.CD != "" {
				return a, usage("--cd given twice")
			}
			if i+1 >= len(args) || args[i+1] == "" {
				return a, usage("--cd needs a directory")
			}
			i++
			a.CD = args[i]
		case "--vars-fd":
			if allowVars {
				return a, usage("--vars-fd is not accepted here")
			}
			if a.VarsFD != 0 {
				return a, usage("--vars-fd given twice")
			}
			if i+1 >= len(args) {
				return a, usage("--vars-fd needs a descriptor")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 3 {
				return a, usage("--vars-fd needs a descriptor number of 3 or more")
			}
			a.VarsFD = n
		case "--vars":
			if !allowVars {
				return a, usage("--vars is not accepted here")
			}
			if a.Vars != "" {
				return a, usage("--vars given twice")
			}
			if i+1 >= len(args) || args[i+1] == "" {
				return a, usage("--vars needs a file name")
			}
			i++
			a.Vars = args[i]
		default:
			return a, usage("unexpected argument before --")
		}
	}
	return a, usage("missing --")
}

// SetprivArgv is the command line of the privilege change. The ids are numeric, so root does
// no name lookup. The calls and what each gives: setgroups([]) drops the supplementary groups;
// the id changes give real, effective and saved ids of the tool user (which also clears the
// permitted, effective and ambient sets); the inheritable, ambient and bounding sets are
// emptied; no_new_privs makes set-id binaries and file capabilities inert.
func SetprivArgv(l Layout, a StageArgs) []string {
	argv := []string{l.Setpriv,
		fmt.Sprintf("--reuid=%d", ToolUID), fmt.Sprintf("--regid=%d", ToolGID), "--clear-groups",
		"--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--no-new-privs",
		l.Artex, CmdToolenv}
	if a.CD != "" {
		argv = append(argv, "--cd", a.CD)
	}
	if a.VarsFD != 0 {
		argv = append(argv, "--vars-fd", strconv.Itoa(a.VarsFD))
	}
	argv = append(argv, "--", a.Program)
	return append(argv, a.Args...)
}

// ExecPlan is what a stage hands to exec.
type ExecPlan struct {
	Path string
	Argv []string
	Env  []string
	Dir  string // the second stage changes into it before exec
	// VarsFile is the root stage's open variables file, nil when the command line named none.
	// The caller makes it an inheritable descriptor and tells the plan the number with SetVarsFD.
	VarsFile *os.File

	layout Layout
	args   StageArgs
}

// SetVarsFD records the descriptor the variables file was moved to and rebuilds the argument
// list so the second stage is told where to read.
func (p *ExecPlan) SetVarsFD(fd int) {
	p.args.VarsFD = fd
	p.Argv = SetprivArgv(p.layout, p.args)
}

// PlanAstool is the root stage: it reads its own environment, keeps the allow-list and the
// fixed values, opens the variables file if one was named, and plans the exec of setpriv. The
// environment goes to setpriv as envp, never as arguments.
func PlanAstool(l Layout, args, environ []string) (*ExecPlan, error) {
	a, err := ParseStageArgs(args, true)
	if err != nil {
		return nil, err
	}
	plan := &ExecPlan{Path: l.Setpriv, Argv: SetprivArgv(l, a), Env: ToolEnv(environ), layout: l, args: a}
	if a.Vars != "" {
		f, err := OpenVarsFile(l.VarsDir, a.Vars, 0)
		if err != nil {
			return nil, &StageError{Code: ExitCannotRun, Msg: "variables file refused", Err: err}
		}
		plan.VarsFile = f
	}
	return plan, nil
}

// LimitProcesses reports whether the root stage sets the tool user's process limit
// (ToolMaxProcesses) before the exec. Every plan does but the kill helper's, the exact command
// line `__astool -- <artex> __killall` the server uses: with the tool user at its limit, the exec
// that follows the change of user fails, and the helper must still start to end those processes.
func (p *ExecPlan) LimitProcesses() bool {
	a := p.args
	killall := a.CD == "" && a.Vars == "" && a.Program == p.layout.Artex && len(a.Args) == 1 && a.Args[0] == CmdKillall
	return !killall
}

// PlanToolenv is the second stage, running as the tool user. open returns the reader for the
// descriptor the root stage named; it is called only when --vars-fd was given, and the reader is
// closed here. A refused key or an unreadable file ends the stage before the program starts.
func PlanToolenv(args, environ []string, open func(fd int) (io.ReadCloser, error)) (*ExecPlan, error) {
	a, err := ParseStageArgs(args, false)
	if err != nil {
		return nil, err
	}
	var kv map[string]string
	if a.VarsFD != 0 {
		r, err := open(a.VarsFD)
		if err != nil {
			return nil, &StageError{Code: ExitDeniedVar, Msg: "variables unreadable", Err: err}
		}
		kv, err = ParseVars(r)
		r.Close()
		if err != nil {
			return nil, &StageError{Code: ExitDeniedVar, Msg: "variables refused", Err: err}
		}
	}
	env := ApplyVars(environ, kv)
	path, err := lookPath(a.Program, pathOf(env))
	if err != nil {
		return nil, &StageError{Code: ExitNotFound, Msg: "program not found: " + a.Program}
	}
	return &ExecPlan{Path: path, Argv: append([]string{a.Program}, a.Args...), Env: env, Dir: a.CD}, nil
}

func pathOf(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return ""
}

// lookPath resolves prog against the given PATH value, not the process's own.
func lookPath(prog, pathValue string) (string, error) {
	if strings.Contains(prog, "/") {
		if executable(prog) {
			return prog, nil
		}
		return "", os.ErrNotExist
	}
	for _, d := range filepath.SplitList(pathValue) {
		if d == "" {
			continue
		}
		if p := filepath.Join(d, prog); executable(p) {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// CheckKillallIdentity refuses to run `__killall` as anyone but the tool user: signalling every
// process (kill -1) as root would end everything in the container.
func CheckKillallIdentity(uid, euid int) error {
	if uid != ToolUID || euid != ToolUID {
		return fmt.Errorf("__killall runs as uid %d only (uid %d, effective %d)", ToolUID, uid, euid)
	}
	return nil
}
