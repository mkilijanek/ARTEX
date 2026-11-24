// Package dbtest is test support for the PostgreSQL tests of ARTEX. It is imported
// by *_test.go files only; no production package may import it, and it never
// imports package db.
//
// The package exists to keep new tests away from every database they did not
// create: the connection string comes from the environment variable ARTEX_PG_DSN
// and from nothing else, and one guard function (Guard) runs before every CREATE
// DATABASE, every fixture load and every DROP DATABASE.
//
// One run per base database at a time. The temporary databases have stable names
// (<base>_upg_<label>, so that the leftover check can find them), and TempDatabase drops a
// database of that name before it creates it: two runs on one server and one base database
// would destroy each other's databases. Give a second run its own database name.
package dbtest

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// EnvDSN is the only source of the test server. There is deliberately no fallback
// to the config file: that file names the operator's own database.
const EnvDSN = "ARTEX_PG_DSN"

// TempMarker separates the package database name from the case name of a
// temporary database: <package database>_upg_<case>.
const TempMarker = "_upg_"

// maxIdentifier is PostgreSQL's identifier limit in bytes.
const maxIdentifier = 63

// packageSuffixes are the suffixes the package TestMain functions add to the
// database name (db.SuffixTestDatabase, #80; SuffixEnv for this package; the sidecar package uses
// _signalsidecar).
var packageSuffixes = []string{"_db", "_dbtest", "_server", "_signalsidecar"}

var caseName = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)

// baseName is what the parsed database name of ARTEX_PG_DSN must match: it is concatenated into
// the names of temporary databases and of the roles of the canary test.
var baseName = regexp.MustCompile(`^[a-z0-9_]+$`)

// Op names what the guard is asked to allow.
type Op string

const (
	OpConnect Op = "connect"
	OpCreate  Op = "create database"
	OpLoad    Op = "load fixture"
	OpDrop    Op = "drop database"

	OpCreateRole Op = "create role"
	OpDropRole   Op = "drop role"
	OpListRoles  Op = "list roles"
)

// Refusal is the error Guard returns. Its message carries the database name the
// driver would use (the parsed name) and a reason; it never carries the DSN or any
// driver error text.
type Refusal struct {
	Op       Op
	Database string // parsed database name; empty when none could be determined
	Reason   string
}

func (r *Refusal) Error() string {
	if r.Database == "" {
		return fmt.Sprintf("dbtest guard: refusing %s: %s", r.Op, r.Reason)
	}
	return fmt.Sprintf("dbtest guard: refusing %s on database %q: %s", r.Op, r.Database, r.Reason)
}

func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func envDSN() string { return strings.TrimSpace(os.Getenv(EnvDSN)) }

// ParsedDatabase returns the database name the driver would connect to: the name
// pgconn.ParseConfig resolves, not the URL path. For a URL-form DSN that carries a
// path, a path that differs from the parsed name (for example because of a
// ?dbname= override) is an error. The error text never contains the DSN.
func ParsedDatabase(dsn string) (string, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		// pgconn echoes (a redacted form of) the DSN in its error: drop it.
		return "", fmt.Errorf("DSN cannot be parsed")
	}
	name := cfg.Database
	if isURL(dsn) {
		u, uerr := url.Parse(dsn)
		if uerr != nil {
			return name, fmt.Errorf("DSN cannot be parsed")
		}
		if path := strings.TrimLeft(u.Path, "/"); path != name {
			return name, fmt.Errorf("the URL path database differs from the database the driver would use")
		}
	}
	return name, nil
}

// Guard decides whether op may run against target, a database on the server that
// dsn names. It must be called before every CREATE DATABASE, every DDL or fixture
// load and every DROP DATABASE of a new test. It refuses unless
//
//   - dsn is, byte for byte, the current value of ARTEX_PG_DSN;
//   - the database name pgconn resolves from dsn agrees with the URL path (URL form);
//   - that name matches [a-z0-9_]+ (it becomes part of the names of temporary databases and
//     roles, which are quoted as identifiers all the same);
//   - that name ends in a package suffix (_db, _dbtest, _server, _signalsidecar), so it is a test
//     database of a package and never the operator's database;
//   - target is empty (a connection to the server only), or is exactly
//     <parsed name>_upg_<case> with <case> in [a-z0-9_] and a total length of at
//     most 63 bytes.
func Guard(op Op, dsn, target string) error {
	env := envDSN()
	if env == "" {
		return &Refusal{Op: op, Reason: EnvDSN + " is not set; new tests take the server from it only and never from the config file"}
	}
	name, perr := ParsedDatabase(dsn)
	refuse := func(reason string) error { return &Refusal{Op: op, Database: name, Reason: reason} }
	if dsn != env {
		return refuse("the DSN is not the value of " + EnvDSN)
	}
	if perr != nil {
		return refuse(perr.Error())
	}
	if strings.Contains(name, TempMarker) {
		return refuse("the base database name already carries the " + TempMarker + " marker")
	}
	if !baseName.MatchString(name) {
		return refuse("the database name is not made of [a-z0-9_] only")
	}
	suffixed := false
	for _, s := range packageSuffixes {
		if strings.HasSuffix(name, s) && len(name) > len(s) {
			suffixed = true
		}
	}
	if !suffixed {
		return refuse("the database name does not end in a package suffix (" + strings.Join(packageSuffixes, ", ") + ")")
	}
	if target == "" {
		return nil
	}
	prefix := name + TempMarker
	if !strings.HasPrefix(target, prefix) || !caseName.MatchString(strings.TrimPrefix(target, prefix)) {
		return refuse(fmt.Sprintf("target %q is not %s<case> with <case> in [a-z0-9_]", target, prefix))
	}
	if len(target) > maxIdentifier {
		return refuse(fmt.Sprintf("target %q exceeds %d bytes", target, maxIdentifier))
	}
	return nil
}
