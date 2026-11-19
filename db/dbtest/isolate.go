package dbtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// EnvConfig names the config file (config.Path reads it first).
const EnvConfig = "ARTEX_CONFIG"

// MustIsolateFromConfigFile is called first in the TestMain of every package whose
// tests reach PostgreSQL through db.DSN(). db.DSN() falls back to the config file
// when ARTEX_PG_DSN is unset, and that file names the operator's own database; a
// test run on such a host would use it (backlog B-1). The function points
// ARTEX_CONFIG at a file that does not exist, so config.Load returns an empty
// configuration, db.DSN() fails with "no database config", and the tests that need
// PostgreSQL skip exactly as they do on a host with no configuration at all.
//
// It overrides ARTEX_CONFIG even when the variable is set: a developer's own
// config file is exactly what the tests must not read. Tests that need a config
// file set ARTEX_CONFIG themselves with t.Setenv.
func MustIsolateFromConfigFile() {
	path := filepath.Join(os.TempDir(), "artex-tests-no-config", "config.json")
	if _, err := os.Stat(path); err == nil {
		panic(fmt.Sprintf("dbtest: %s exists; remove it, tests rely on it being absent", path))
	} else if !errors.Is(err, os.ErrNotExist) {
		panic(fmt.Sprintf("dbtest: stat %s: %v", path, err))
	}
	if err := os.Setenv(EnvConfig, path); err != nil {
		panic(fmt.Sprintf("dbtest: set %s: %v", EnvConfig, err))
	}
}

// ChildResult is the outcome of RunChild.
type ChildResult struct {
	Output   string // combined stdout and stderr of the child
	ExitCode int
	Dials    int // connections the marker listener accepted
}

// RunChild re-executes the running test binary for the tests that match
// runPattern (an anchored regular expression is added), in a child whose
// environment has no ARTEX_PG_DSN and an ARTEX_CONFIG naming a config file whose
// database.dsn points at a listener this function owns (the marker). The child
// runs the package TestMain as usual. The listener accepts and counts connections
// and closes them at once; Dials is therefore the number of times the child tried
// to reach "the database from the config file". extraEnv entries are added to the
// child's environment.
func RunChild(t *testing.T, runPattern string, extraEnv ...string) ChildResult {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("marker listener: %v", err)
	}
	var dials atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			c.Close()
		}
	}()
	defer func() {
		ln.Close()
		<-done
	}()

	cfg, err := json.Marshal(map[string]any{"database": map[string]any{
		"dsn": fmt.Sprintf("postgres://marker:marker@%s/marker_db?sslmode=disable&connect_timeout=2", ln.Addr()),
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^("+runPattern+")$", "-test.v", "-test.count=1")
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, EnvDSN+"=") || strings.HasPrefix(kv, EnvConfig+"=") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, EnvConfig+"="+cfgPath)
	cmd.Env = append(cmd.Env, extraEnv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	res := ChildResult{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run child test binary: %v", err)
		}
		res.ExitCode = ee.ExitCode()
	}
	res.Output = out.String()
	res.Dials = int(dials.Load())
	return res
}
