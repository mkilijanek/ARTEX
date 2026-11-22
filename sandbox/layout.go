// Package sandbox holds the privilege separation for model-driven processes: the drop chain
// that every tool process starts through, the lease that keeps the idle sweep from killing a
// live process, the start-up checks and their status, and the permission step.
//
// The drop chain is shim -> `artex __astool` (root, builds a clean environment) -> setpriv
// (changes uid, empties the capability sets, sets no_new_privs) -> `artex __toolenv` (tool
// user, applies the per-call variables) -> the program. Every stage execs, so the process id
// and process group of the first stage are the ones the caller sees.
package sandbox

// Fixed values of the tool user and the image layout.
const (
	ToolUID  = 30002
	ToolGID  = 30002
	ToolUser = "artex-tool"
	ToolHome = "/home/artex-tool"
	ToolPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	// ToolMaxProcesses is the soft and hard RLIMIT_NPROC the root stage sets before the drop: the
	// most processes the tool user can have, so a fork loop of a tool ends there instead of at
	// the container's process limit. The kernel counts every process of uid 30002 in the user
	// namespace (all sessions together, plus anything started as that uid from outside), so it
	// is well above what the browser and parallel tool shells need. RLIMIT_AS is not set:
	// Chromium reserves large address ranges it never uses.
	ToolMaxProcesses = 4096

	// ArtexBin must be a static executable: the root stage runs it, and a static binary never
	// starts the dynamic loader, so no loader variable can affect it.
	ArtexBin   = "/app/artex"
	SetprivBin = "/usr/bin/setpriv"
	ShimDir    = "/usr/local/libexec/artex/shims"
	PrivateDir = "/run/artex-private"
	VarsDir    = "/run/artex-private/vars"
	ServerTmp  = "/run/artex-tmp"

	// Subcommands of the artex binary. They are named here and nowhere else, so the
	// architecture test can forbid a spawn that spells one out.
	CmdAstool  = "__astool"
	CmdToolenv = "__toolenv"
	CmdKillall = "__killall"
	// CmdToolfs runs one file tool (Read, Write, Edit, MultiEdit, LS, Glob, Grep) as the tool
	// user: the server starts it through the drop chain and talks to it over stdin and stdout.
	CmdToolfs = "__toolfs"
	// CmdMigrateWorkspaces is the runbook command that hands the contents of every workspace to the
	// tool user without the bounds of a session start. It runs as root, by hand, with the server
	// stopped; it is not part of the drop chain.
	CmdMigrateWorkspaces = "__migrate-workspaces"

	// EnvIsolation is the operator's override. It is read from the environment only; an empty
	// value means unset.
	EnvIsolation = "ARTEX_TOOL_ISOLATION"
)

// Layout names the places the package reads and writes. Production uses DefaultLayout; tests
// point it at a temporary tree.
type Layout struct {
	Artex      string // static binary the root stage runs
	Setpriv    string
	ShimDir    string
	PrivateDir string // root 0700: database socket mount, variables files, chain heads
	VarsDir    string
	ServerTmp  string // TMPDIR of the server process
	ToolHome   string
	ProcRoot   string // /proc
}

// DefaultLayout is the layout of the shipped image.
func DefaultLayout() Layout {
	return Layout{
		Artex: ArtexBin, Setpriv: SetprivBin, ShimDir: ShimDir, PrivateDir: PrivateDir,
		VarsDir: VarsDir, ServerTmp: ServerTmp, ToolHome: ToolHome, ProcRoot: "/proc",
	}
}
