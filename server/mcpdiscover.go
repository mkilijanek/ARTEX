package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/mcphttp"
	"github.com/mkilijanek/artex/sandbox"
)

// mcpClient is the shared surface of a connected MCP server (stdio, Streamable HTTP,
// or legacy SSE),
// so tools/list and cleanup are handled uniformly regardless of transport.
type mcpClient interface {
	Tools(context.Context) ([]actool.CoreTool, error)
	Close() error
}

// newStdioClient is the SDK's stdio client. connectStdioMCP is the only caller: the architecture
// test forbids the reference anywhere else, so every MCP process starts through that function.
var newStdioClient = mcp.NewStdioClient

// connectMCP dials one MCP server per its transport. Callers must Close the client.
func connectMCP(ctx context.Context, m *db.MCPServer) (mcpClient, error) {
	switch m.Transport {
	case "stdio":
		if m.Command == "" {
			return nil, fmt.Errorf("stdio transport is missing a command")
		}
		return connectStdioMCP(ctx, m)
	case "http":
		if m.URL == "" {
			return nil, fmt.Errorf("http transport is missing a URL")
		}
		// env map doubles as HTTP headers (e.g. Authorization).
		return mcphttp.New(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure, m.TimeoutSeconds)
	case "sse":
		if m.URL == "" {
			return nil, fmt.Errorf("sse transport is missing a URL")
		}
		return mcphttp.NewSSE(ctx, m.Name, m.URL, jsonStrMap(m.Env), m.Insecure)
	default:
		return nil, fmt.Errorf("unknown transport %q", m.Transport)
	}
}

// discoverAndCacheMCP connects to one MCP, lists its tools, and persists the tool
// names to mcp_tools_cache so the UI shows them without a live connection.
func (s *Server) discoverAndCacheMCP(ctx context.Context, m *db.MCPServer) error {
	cl, err := connectMCP(ctx, m)
	if err != nil {
		_ = s.m.pg.UpdateMCPHealth(m.ID, false, err.Error())
		return err
	}
	defer cl.Close()
	ts, err := cl.Tools(ctx)
	if err != nil {
		_ = s.m.pg.UpdateMCPHealth(m.ID, false, err.Error())
		return err
	}
	_ = s.m.pg.UpdateMCPHealth(m.ID, true, "")
	tools := make([]db.MCPTool, 0, len(ts))
	for _, t := range ts {
		tools = append(tools, db.MCPTool{Name: t.Name(), Description: t.Description()})
	}
	if err := s.m.pg.SaveMCPTools(m.ID, tools); err != nil {
		return err
	}
	log.Printf("[mcp] %s discovered %d tools and cached them", m.Name, len(tools))
	return nil
}

// discoverEmptyMCPsOnStartup fills the tool cache for any enabled MCP that has none
// yet (notably the seeded browser MCP on first run). Runs sequentially in one
// goroutine so we never spawn many stdio servers (npx) at once, and never blocks
// startup. Best-effort: a failure leaves the cache empty to retry next start.
func (s *Server) discoverEmptyMCPsOnStartup() {
	servers, err := s.m.pg.ListMCP()
	if err != nil {
		log.Printf("[mcp] startup auto-discovery: failed to read the list: %v", err)
		return
	}
	for _, m := range servers {
		if !m.Enabled || len(m.Tools) > 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
		if err := s.discoverAndCacheMCP(ctx, m); err != nil {
			log.Printf("[mcp] startup auto-discovery of %s failed: %v", m.Name, err)
		}
		cancel()
	}
}

// leasedMCPClient holds a tool-process lease for the life of a stdio client, so the idle sweep
// cannot end a server that is in use. Close releases it.
type leasedMCPClient struct {
	mcpClient
	lease *sandbox.Lease
}

func (c *leasedMCPClient) Close() error {
	err := c.mcpClient.Close()
	c.lease.Release()
	return err
}

// connectStdioMCP starts a stdio MCP server. It is the MCP spawn function: the one place the SDK's
// stdio client is used.
//
// The environment map of the row is model-writable (create_mcp, update_mcp), so its keys are
// validated here, before anything is written, in every isolation mode. With isolation on the
// server starts through the drop chain in a directory of the tool user's home; the map is not
// given to the SDK (the root stage would see it) but travels in a root-only file that the chain
// reads after the user change. A refusal is logged with the key name only and returned, so the MCP
// status shows it.
func connectStdioMCP(ctx context.Context, m *db.MCPServer) (mcpClient, error) {
	iso := sandbox.Current()
	env := jsonStrMap(m.Env)
	if err := sandbox.ValidateVars(env); err != nil {
		var de *sandbox.DeniedVarError
		if errors.As(err, &de) {
			log.Printf("[mcp] refused server %s: denied variable name %s", logSafe(m.Name), logSafe(de.Key))
		} else {
			log.Printf("[mcp] refused server %s: %v", logSafe(m.Name), err)
		}
		return nil, err
	}
	lease, err := iso.Leases().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	spec := sandbox.Spec{Program: m.Command, Args: jsonStrSlice(m.Args), Vars: env}
	if iso.On() {
		dir, err := sandbox.EnsureToolDir(iso.Layout().ToolHome, "mcp", mcpDirName(m.Name))
		if err != nil {
			lease.Release()
			return nil, fmt.Errorf("prepare the working directory of MCP server %s: %w", logSafe(m.Name), err)
		}
		spec.Dir = dir
	}
	program, args, cleanup, err := iso.ChainArgs(lease, spec)
	if err != nil {
		lease.Release()
		return nil, err
	}
	var stdEnv map[string]string
	if !iso.On() {
		stdEnv = env // as before: the SDK merges the map into the child's environment
	}
	c, err := newStdioClient(ctx, m.Name, program, stdEnv, args...)
	cleanup() // the chain unlinked the file when it opened it; this covers a spawn that never got there
	if err != nil {
		lease.Release()
		return nil, err
	}
	return &leasedMCPClient{mcpClient: c, lease: lease}, nil
}

var safeDirName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// mcpDirName turns a server name into a directory name. The name is model-chosen, so one that is
// not a plain name gets a name derived from its hash instead of choosing a path.
func mcpDirName(name string) string {
	if safeDirName.MatchString(name) {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return "x-" + hex.EncodeToString(sum[:8])
}

// logSafe keeps a model-chosen string from forging log lines: control characters become '?' and
// the length is capped.
func logSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
