// Command artex-signal is the Signal sidecar of ARTEX. It owns signal-cli as a
// child process, takes its commands from the database (as a role that can read
// one view and call three functions) and sends messages for ARTEX over a
// private Unix socket.
//
//	artex-signal               run the sidecar
//	artex-signal healthcheck   ask the running sidecar on its socket; no database
//	artex-signal version       print the version
//
// Environment, read once and then removed from the process environment:
//
//	ARTEX_SIGNAL_PG_DSN  connection string of the sidecar role (required)
//	ARTEX_SIGNAL_SOCKET  socket of the API (default /sock/api.sock)
//	ARTEX_SIGNAL_DATA    data directory of signal-cli (default /data)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mkilijanek/artex/signalsidecar"
	"golang.org/x/sys/unix"
)

// version is set at build time.
var version = "dev"

const (
	envDSN    = "ARTEX_SIGNAL_PG_DSN"
	envSocket = "ARTEX_SIGNAL_SOCKET"
	envData   = "ARTEX_SIGNAL_DATA"

	defaultSocket = "/sock/api.sock"
	defaultData   = "/data"

	// exitConfig is the exit status of a refusal to run with this setup
	// (sysexits EX_CONFIG).
	exitConfig = 78

	serviceName = "artex-signal"

	// childDownGrace is how long signal-cli may be down or silent before the
	// health check fails; a restart with backoff fits inside it.
	childDownGrace = 2 * time.Minute
)

// Fixed places and values of the image; the child never gets more than these.
const (
	javaBinary = "/usr/bin/java"
	nativeDir  = "/opt/signal-cli/native"
	libDir     = "/opt/signal-cli/lib"
	childPath  = "/usr/bin:/bin"
	childLang  = "C.UTF-8"
	childTmp   = "/tmp"
)

// configError is a refusal to run: a setup that is unsafe or unusable.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

type settings struct {
	dsn, socket, data string
}

func readSettings(getenv func(string) string) (settings, error) {
	s := settings{dsn: getenv(envDSN), socket: getenv(envSocket), data: getenv(envData)}
	if s.dsn == "" {
		return s, &configError{envDSN + " is required"}
	}
	if s.socket == "" {
		s.socket = defaultSocket
	}
	if s.data == "" {
		s.data = defaultData
	}
	return s, nil
}

// hooks are the places where a test replaces what the image fixes.
type hooks struct {
	parseDSN func(string) (*pgx.ConnConfig, error)
	// child returns the command line of signal-cli for a data directory.
	child    func(dataDir string) (command string, args []string)
	childEnv []string
	logf     func(format string, args ...any)
}

func defaultHooks() hooks {
	return hooks{
		parseDSN: signalsidecar.ParseStoreConfig,
		child: func(dataDir string) (string, []string) {
			return signalsidecar.JavaCommand(javaBinary, nativeDir, libDir, dataDir)
		},
		childEnv: signalsidecar.ChildEnv(childTmp, childPath, childLang, childTmp),
		logf:     log.Printf,
	}
}

func main() {
	log.SetFlags(0)
	os.Exit(realMain(os.Args[1:], os.Getenv, os.Unsetenv))
}

func realMain(args []string, getenv func(string) string, unsetenv func(string) error) int {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "version":
		fmt.Println(version)
		return 0
	case "healthcheck":
		socket := getenv(envSocket)
		if socket == "" {
			socket = defaultSocket
		}
		if err := healthcheck(socket); err != nil {
			log.Printf("[signal] health check failed: %v", err)
			return 1
		}
		return 0
	case "":
	default:
		log.Printf("[signal] unknown command %q; use healthcheck or version, or no argument to run", cmd)
		return 2
	}

	// Unsetting a variable does not change /proc/<pid>/environ, which holds the
	// initial environment, and the child runs as the same user. A process that
	// is not dumpable makes its /proc files root-owned, so the child cannot read
	// the password from them. This runs before anything is started.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		log.Printf("[signal] cannot make the process non-dumpable: %v", err)
		return 1
	}
	s, err := readSettings(getenv)
	if err != nil {
		log.Printf("[signal] %v", err)
		return exitConfig
	}
	// The child gets an explicit environment; nothing else in this process
	// needs these values once they are read.
	for _, k := range []string{envDSN, envSocket, envData} {
		if err := unsetenv(k); err != nil {
			log.Printf("[signal] cannot remove %s from the environment: %v", k, err)
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := serve(ctx, s, defaultHooks()); err != nil {
		log.Printf("[signal] %v", err)
		var ce *configError
		if errors.As(err, &ce) {
			return exitConfig
		}
		return 1
	}
	return 0
}

// probeBackoffMax bounds the wait between two inconclusive password probes.
const probeBackoffMax = 15 * time.Second

// checkPasswords runs the wrong-password probe until it has a verdict. A
// database that cannot be reached yet is waited for, not given up on.
func checkPasswords(ctx context.Context, cc *pgx.ConnConfig, logf func(string, ...any)) error {
	wait := time.Second
	for {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := signalsidecar.ProbeWrongPassword(pctx, cc)
		cancel()
		switch {
		case err == nil:
			return nil
		case errors.Is(err, signalsidecar.ErrPasswordNotChecked):
			return &configError{"the database accepts a login with a wrong password; refusing to start signal-cli " +
				"(use password authentication for the sidecar role)"}
		case ctx.Err() != nil:
			return ctx.Err()
		}
		logf("[signal] the database password check cannot be tested yet: %v", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, probeBackoffMax)
	}
}

// serve runs the sidecar until ctx ends. The order is fixed: refuse an unsafe
// address, refuse a database that does not check passwords, wait until the
// server has confirmed the authentication rules, and only then open the socket
// and start signal-cli.
func serve(ctx context.Context, s settings, h hooks) error {
	cc, err := h.parseDSN(s.dsn)
	if err != nil {
		return &configError{err.Error()}
	}
	if err := checkPasswords(ctx, cc, h.logf); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	store := signalsidecar.NewStore(cc, h.logf)
	defer store.Close()
	command, args := h.child(s.data)
	sup := signalsidecar.NewSupervisor(signalsidecar.Config{Command: command, Args: args, Env: h.childEnv, Logf: h.logf})
	machine := signalsidecar.NewMachine(signalsidecar.MachineConfig{
		DataDir: s.data, SidecarVersion: version, Logf: h.logf,
	}, store, sup)

	if err := machine.AwaitAuth(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	ln, err := signalsidecar.ListenSocket(s.socket)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           signalsidecar.NewAPI(machine, h.logf),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("socket API: %w", err)
			cancelRun()
		}
		close(serveErr)
	}()

	supCtx, cancelSup := context.WithCancel(context.Background())
	supDone := make(chan error, 1)
	go func() { supDone <- sup.Run(supCtx) }()

	runErr := machine.Run(runCtx)

	// Stop accepting, give a running send up to 10 s, then stop signal-cli.
	shCtx, cancelSh := context.WithTimeout(context.Background(), 10*time.Second)
	if err := srv.Shutdown(shCtx); err != nil {
		h.logf("[signal] a send was still running after 10 s: %v", err)
		_ = srv.Close()
	}
	cancelSh()
	cancelSup()
	if err := <-supDone; err != nil && runErr == nil {
		runErr = err
	}
	if err := <-serveErr; err != nil && runErr == nil {
		runErr = err
	}
	return runErr
}

// healthcheck asks the running sidecar for GET /v1/about on its socket. It
// opens no database connection.
func healthcheck(socket string) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get("http://sidecar/v1/about")
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var about signalsidecar.About
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&about); err != nil {
		return fmt.Errorf("answer is not valid: %w", err)
	}
	if about.Service != serviceName {
		return errors.New("the socket belongs to another service")
	}
	if about.ChildDownSeconds > int64(childDownGrace/time.Second) {
		return fmt.Errorf("signal-cli has been down or silent for %d s", about.ChildDownSeconds)
	}
	return nil
}
