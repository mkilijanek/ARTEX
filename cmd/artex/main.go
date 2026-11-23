// Command artex runs the ARTEX backend: the dual SQLite graph stores,
// the event-driven exploration engine, and the JSON HTTP API consumed by the
// shadcn/ui frontend.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/config"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/notify"
	"github.com/mkilijanek/artex/sandbox"
	"github.com/mkilijanek/artex/server"
	"github.com/mkilijanek/artex/traffic"
)

// version is the build version, injected at release time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

const banner = `
    _    ____ _____ _______  __
   / \  |  _ \_   _| ____\ \/ /
  / _ \ | |_) || | |  _|  \  /
 / ___ \|  _ < | | | |___ /  \
/_/   \_\_| \_\|_| |_____/_/\_\
`

// printBanner writes the startup banner + version/runtime info to stdout.
func printBanner(addr string) {
	fmt.Print(banner)
	fmt.Println("  AI autonomous penetration testing system")
	fmt.Printf("  version %s  ·  %s/%s  ·  %s  ·  listening on %s\n\n",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), addr)
}

// main maps run's result onto the process exit code. The body lives in a
// function that can *return* rather than os.Exit, so its own defers still run.
func main() {
	os.Exit(run())
}

func run() int {
	// The stages of the drop chain are subcommands of this binary. They run before anything
	// else (no flags, no logging, no banner), so the root stage touches nothing it need not.
	if len(os.Args) > 1 {
		if code, ok := runStage(os.Args[1], os.Args[2:]); ok {
			return code
		}
	}
	var (
		addr    = flag.String("addr", ":8787", "HTTP listen address")
		dataDir = flag.String("data", filepath.Join(config.BaseDir(), "data"), "data directory for SQLite stores (default: data/ next to the executable)")
		proxy   = flag.String("proxy", "127.0.0.1:8788", "traffic recording proxy address (empty to disable)")
		egress  = flag.String("egress", traffic.DefaultEgressAddr, "credential-free egress forwarder for tools, loopback address only (empty to disable)")
	)
	flag.Parse()

	// hand the build version to the server package so GET /api/health can report it
	// to the frontend top bar.
	server.BuildVersion = version

	printBanner(*addr)

	// capture backend logs into the in-memory sink (still to stderr) so the /logs
	// page can show a live log stream. Do this first, to catch startup logs too.
	server.StartLogCapture()

	// surface which config file the binary reads (absolute, so `go run`'s relative
	// "config.json" — resolved against the CWD — is unambiguous).
	cfgPath := config.Path()
	if abs, e := filepath.Abs(cfgPath); e == nil {
		cfgPath = abs
	}
	if _, e := os.Stat(cfgPath); e == nil {
		log.Printf("[config] config file: %s", cfgPath)
	} else {
		log.Printf("[config] config file: %s (not found — will only try the environment variable ARTEX_PG_DSN)", cfgPath)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, shutdown := shutdownContext(sigCtx)
	defer shutdown(agent.AbortShutdown)

	skillDir, err := config.SkillDir()
	if err != nil {
		log.Printf("[config] %v", err)
		return 1
	}
	if abs, err := filepath.Abs(skillDir); err == nil {
		skillDir = abs
	}
	log.Printf("[config] skill directory: %s", skillDir)

	// Activation order. Step 1: read every secret once, then remove it from the environment.
	// Nothing below can start a tool process before this is done, and isolation start-up refuses
	// to go on while any of them is still set.
	dsn, dsnSource, err := db.DSN()
	if err != nil {
		log.Fatalf("open stores: %v", err)
	}
	log.Printf("[pg] database config source: %s", dsnSource)
	dbEndpoints, endpointsErr := agent.DatabaseEndpoint(dsn)
	if endpointsErr != nil {
		// The text carries no part of the DSN. Without the endpoints WebFetch refuses every fetch.
		log.Printf("[webfetch-policy] database endpoints unavailable: %v", endpointsErr)
	}
	envLLM, envLLMOK := agent.FromEnv()
	server.ScrubSecretEnv()

	// Steps 2-7: kill leftovers, TMPDIR, PATH, permissions, the self-check, the mode.
	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Printf("[config] data directory: %v", err)
		return 1
	}
	iso, err := sandbox.Activate(ctx, sandbox.Config{
		Layout: sandbox.DefaultLayout(), DataDir: absData, KeyDir: config.BaseDir(), ConfigPath: config.Path(), SkillDir: skillDir,
		DBSockets: databaseSockets(dbEndpoints), DBTCP: databaseTCP(dbEndpoints), SecretNames: server.SecretEnvNames(),
		SignalSocket: notify.SignalSocketPath(),
	})
	if err != nil {
		log.Printf("[isolation] start-up failed: %v", err)
		return 1
	}
	sandbox.Set(iso)
	notify.SetSignalRecheck(iso.RecheckSignal) // the self-check runs once: C12 is re-run after a Signal send or test
	go iso.Run(ctx)
	agent.SweepSpill(absData) // old spill directories, before any session runs

	mgr, err := server.NewManager(*dataDir, *proxy, dsn)
	if err != nil {
		log.Fatalf("open stores: %v", err)
	}
	defer mgr.Close()

	// The forwarder tools reach the operator's global egress proxy through. If it cannot start, tools
	// are still pointed at its address (their egress fails visibly instead of using the credentials)
	// and the status becomes degraded with the reason.
	if err := mgr.StartEgressForwarder(*egress); err != nil {
		log.Printf("[egress] %v", err)
		iso.ReportFailure("E1", "the egress forwarder is not running ("+err.Error()+"): tools cannot use the global egress proxy")
	}

	options := []server.Option{server.WithControlPlane(*addr, *proxy), server.WithEnvLLM(envLLM, envLLMOK)}
	if endpointsErr == nil {
		options = append(options, server.WithDatabaseEndpoints(dbEndpoints))
	}
	srv := server.New(ctx, mgr, skillDir, *dataDir, config.BaseDir(), options...)
	iso.ReapplyPermissions() // entries the store and the server created while starting
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("ARTEX %s backend listening on %s (data=%s, workers=%d)", version, *addr, *dataDir, mgr.Workers())
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Wait for a stop signal.
	<-ctx.Done()

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	// Graceful stop: wait for the tool-process leases, then end whatever the tool user still runs.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer drainCancel()
	iso.Drain(drainCtx)
	return 0
}

// databaseSockets lists the socket files the DSN's endpoints name (a host that is a path), for
// the self-check's reachability probe.
func databaseSockets(eps []agent.DBEndpoint) []string {
	var out []string
	for _, e := range eps {
		if strings.HasPrefix(e.Host, "/") {
			out = append(out, fmt.Sprintf("%s/.s.PGSQL.%d", strings.TrimRight(e.Host, "/"), e.Port))
		}
	}
	return out
}

// databaseTCP lists the host:port endpoints the DSN reaches over the network.
func databaseTCP(eps []agent.DBEndpoint) []string {
	var out []string
	for _, e := range eps {
		if !strings.HasPrefix(e.Host, "/") && !strings.HasPrefix(e.Host, "@") {
			out = append(out, net.JoinHostPort(e.Host, strconv.Itoa(int(e.Port))))
		}
	}
	return out
}

// shutdownContext deliberately does not derive from signalCtx. If it did, the
// parent's plain context.Canceled could win the race before AbortShutdown was
// attached to the child, losing the diagnostic cause in every running Agent.
func shutdownContext(signalCtx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, shutdown := context.WithCancelCause(context.Background())
	go func() {
		select {
		case <-signalCtx.Done():
			shutdown(agent.AbortShutdown)
		case <-ctx.Done():
		}
	}()
	return ctx, shutdown
}
