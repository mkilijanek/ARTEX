package dbtest

import (
	"bytes"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The canaries make a leak visible. Fixture rows carry RowCanary in their text
// columns. PasswordCanary is a fixed, recognisable string for places where it never
// becomes a credential (a wrong password in a DSN, the password in a DSN of a unit test);
// a PostgreSQL role the tests create takes a random password of its own, made per run and
// registered with RegisterSecret (CreateCanaryRole). None of these strings may appear in
// any log line, failure message, guard refusal or dry-run report (AC12).
const (
	RowCanary      = "CANARY-D2-ROW"
	PasswordCanary = "CANARY-D2-PW"
)

// minSecretLen keeps a short real password from matching ordinary words.
const minSecretLen = 8

type secret struct{ kind, value string }

var (
	registeredMu sync.Mutex
	registered   []*secret
)

// RegisterSecret adds a value to the strings FindSecret looks for, until remove is called.
// A value shorter than minSecretLen is refused: it would match ordinary words. Several
// values may be registered under one kind.
func RegisterSecret(kind, value string) (remove func(), err error) {
	if len(value) < minSecretLen {
		return nil, fmt.Errorf("a secret to register must be at least %d bytes long", minSecretLen)
	}
	entry := &secret{kind: kind, value: value}
	registeredMu.Lock()
	defer registeredMu.Unlock()
	registered = append(registered, entry)
	return func() {
		registeredMu.Lock()
		defer registeredMu.Unlock()
		for i, e := range registered {
			if e == entry {
				registered = append(registered[:i:i], registered[i+1:]...)
				return
			}
		}
	}, nil
}

// secrets are the strings that must not appear in test output: both canaries, the
// registered secrets and the password of the server named by ARTEX_PG_DSN, when it has one.
func secrets() []secret {
	m := []secret{
		{"row canary", RowCanary},
		{"password canary", PasswordCanary},
	}
	registeredMu.Lock()
	for _, e := range registered {
		m = append(m, *e)
	}
	registeredMu.Unlock()
	if dsn := envDSN(); dsn != "" {
		if cfg, err := pgconn.ParseConfig(dsn); err == nil && len(cfg.Password) >= minSecretLen {
			m = append(m, secret{"ARTEX_PG_DSN password", cfg.Password})
		}
	}
	return m
}

// FindSecret reports which kind of secret, if any, text contains. It returns the
// kind only, never the secret.
func FindSecret(text string) (kind string, found bool) {
	for _, s := range secrets() {
		if strings.Contains(text, s.value) {
			return s.kind, true
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
