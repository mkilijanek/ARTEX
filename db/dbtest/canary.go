package dbtest

import (
	"bytes"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The canaries make a leak visible. Fixture rows carry RowCanary in their text
// columns; the PostgreSQL role the canary tests use has PasswordCanary as its
// password. Neither string may appear in any log line, failure message, guard
// refusal or dry-run report (AC12).
const (
	RowCanary      = "CANARY-D2-ROW"
	PasswordCanary = "CANARY-D2-PW"
)

// minSecretLen keeps a short real password from matching ordinary words.
const minSecretLen = 8

// secrets are the strings that must not appear in test output: both canaries and
// the password of the server named by ARTEX_PG_DSN, when it has one.
func secrets() map[string]string {
	m := map[string]string{
		"row canary":      RowCanary,
		"password canary": PasswordCanary,
	}
	if dsn := envDSN(); dsn != "" {
		if cfg, err := pgconn.ParseConfig(dsn); err == nil && len(cfg.Password) >= minSecretLen {
			m["ARTEX_PG_DSN password"] = cfg.Password
		}
	}
	return m
}

// FindSecret reports which kind of secret, if any, text contains. It returns the
// kind only, never the secret.
func FindSecret(text string) (kind string, found bool) {
	for kind, s := range secrets() {
		if strings.Contains(text, s) {
			return kind, true
		}
	}
	return "", false
}

// AssertNoSecret fails the test when text contains a canary or the server
// password. The failure message names what was checked and which kind of secret
// matched; it does not repeat text, so a failing check cannot itself leak.
func AssertNoSecret(t testing.TB, what, text string) {
	t.Helper()
	if kind, found := FindSecret(text); found {
		t.Fatalf("%s contains the %s (%d bytes checked)", what, kind, len(text))
	}
}

// CaptureLog redirects the standard logger into a buffer until the test ends and
// returns a function that yields what was logged so far.
func CaptureLog(t testing.TB) func() string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return buf.String
}

// SuffixEnv gives this package's own tests a database name of their own, as
// db.SuffixTestDatabase does for the other packages (it cannot be used here:
// dbtest never imports package db). Only a URL-form DSN with a database path is
// rewritten; anything else is left unchanged for the guard to refuse.
func SuffixEnv(suffix string) {
	dsn := envDSN()
	if dsn == "" || !isURL(dsn) {
		return
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Path == "" || u.Path == "/" {
		return
	}
	u.Path = "/" + strings.TrimPrefix(u.Path, "/") + "_" + suffix
	u.RawPath = ""
	if err := os.Setenv(EnvDSN, u.String()); err != nil {
		panic(fmt.Sprintf("dbtest: set %s: %v", EnvDSN, err))
	}
}
