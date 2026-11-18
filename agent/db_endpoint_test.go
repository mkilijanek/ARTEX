package agent

import (
	"net/url"
	"strings"
	"testing"
)

// Review finding 3 (PR 1): the denylist covers every (host, port) pair pgx may connect to,
// not just the first host and one port. Hermetic: the DSN is pinned, names resolve through
// an injected resolver and the interface list is fake.

const (
	dbPrimary = "db-primary.example"
	dbStandby = "db-standby.example"
)

// multiHostDSNs spell the same two endpoints, primary:5432 and standby:6432, in every form
// pgx accepts.
var multiHostDSNs = map[string]string{
	"url query lists":          "postgres://artex:pw@/artex?host=" + dbPrimary + "," + dbStandby + "&port=5432,6432&sslmode=disable",
	"url authority list":       "postgres://artex:pw@" + dbPrimary + ":5432," + dbStandby + ":6432/artex?sslmode=disable",
	"keyword/value":            "host=" + dbPrimary + "," + dbStandby + " port=5432,6432 user=artex password=pw dbname=artex sslmode=disable",
	"fallback tls (prefer)":    "postgres://artex:pw@" + dbPrimary + ":5432," + dbStandby + ":6432/artex?sslmode=prefer",
	"authority and query port": "postgres://artex:pw@" + dbPrimary + "," + dbStandby + "/artex?port=5432,6432&sslmode=disable",
}

// clearPGEnv keeps the PG* defaults pgx reads (PGHOST, PGPORT, ...) out of the parse.
func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGHOST", "PGPORT", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGPASSFILE", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGTARGETSESSIONATTRS"} {
		t.Setenv(k, "")
	}
}

func checkDest(t *testing.T, dest string) error {
	t.Helper()
	u, err := url.Parse(dest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewTargetGuard("", false).Check(WithRunInfo(t.Context(), policyRI), u)
	return err
}

func requireControlPlane(t *testing.T, dest string, want bool) {
	t.Helper()
	err := checkDest(t, dest)
	var te *TargetError
	switch {
	case want && !(asTargetError(err, &te) && te.Code == codeControlPlane):
		t.Errorf("%s: want %s, got %v", dest, codeControlPlane, err)
	case !want && err != nil:
		t.Errorf("%s: want it allowed, got %v", dest, err)
	}
}

func asTargetError(err error, te **TargetError) bool {
	for err != nil {
		if e, ok := err.(*TargetError); ok {
			*te = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestWebFetchPolicyDeniesEveryDatabaseEndpoint(t *testing.T) {
	for name, dsn := range multiHostDSNs {
		t.Run(name, func(t *testing.T) {
			clearPGEnv(t)
			t.Setenv("ARTEX_PG_DSN", dsn)
			res := newFakeResolver(map[string][]string{dbPrimary: {"203.0.113.10"}, dbStandby: {"203.0.113.11"}})
			setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", res), false)
			for _, dest := range []string{
				"http://203.0.113.10:5432/",      // primary on its port
				"http://203.0.113.11:6432/",      // fallback endpoint on its port
				"http://" + dbPrimary + ":5432/", // by name
				"http://" + dbStandby + ":6432/", // by name
				"http://127.0.0.1:5432/",         // local address on every database port
				"http://127.0.0.1:6432/",         //
				"http://10.77.0.5:6432/",         // interface address on the fallback port
				"http://[::1]:6432/",             //
				"http://127.0.0.1:06432/",        // zero-padded spelling of the port
				"http://203.0.113.11:06432/",     //
			} {
				requireControlPlane(t, dest, true)
			}
			for _, dest := range []string{
				"http://203.0.113.10:6432/", // the pairs are per endpoint
				"http://203.0.113.11:5432/",
				"http://127.0.0.1:7000/",
				"http://" + dbPrimary + ":6432/",
			} {
				requireControlPlane(t, dest, false)
			}
		})
	}
}

// A host the resolver cannot answer keeps its name and the local-address pairs denied, and
// logs one content-free line per host.
func TestWebFetchPolicyUnresolvedDatabaseHostsStayDenied(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("ARTEX_PG_DSN", multiHostDSNs["url query lists"])
	setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", newFakeResolver(map[string][]string{})), true)
	logs := captureLog(t)
	for _, dest := range []string{"http://" + dbPrimary + ":5432/", "http://" + dbStandby + ":6432/", "http://127.0.0.1:6432/"} {
		requireControlPlane(t, dest, true)
	}
	requireControlPlane(t, "http://"+dbPrimary+":6432/", false)
	out := logs.String()
	if n := strings.Count(out, "[webfetch-policy] db_host_unresolved task=7"); n < 2 {
		t.Fatalf("want a db_host_unresolved line per host, got %d in %q", n, out)
	}
	for _, bad := range []string{"db-primary", "db-standby", "5432", "6432", "pw"} {
		if strings.Contains(strings.ReplaceAll(out, "task=7", ""), bad) {
			t.Fatalf("log carries %q: %q", bad, out)
		}
	}
}

// A unix-socket host is not a network endpoint, but it means PostgreSQL runs on this host
// and usually listens on TCP loopback too: its port stays denied on local addresses (and
// only there). An unset DSN protects nothing.
func TestWebFetchPolicySocketDatabaseHostKeepsItsLocalPortDenied(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("ARTEX_PG_DSN", "")
	setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", nil), false)
	requireControlPlane(t, "http://127.0.0.1:5432/", false) // no DSN, nothing to protect
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@/artex?host=/var/run/postgresql&sslmode=disable")
	setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", nil), false)
	requireControlPlane(t, "http://127.0.0.1:5432/", true)
	requireControlPlane(t, "http://10.77.0.5:5432/", true)
	requireControlPlane(t, "http://127.0.0.1:5433/", false)
	requireControlPlane(t, "http://203.0.113.11:5432/", false) // the socket says nothing about remote hosts
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@/artex?host=/var/run/postgresql,"+dbStandby+"&port=5432,6432&sslmode=disable")
	setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", newFakeResolver(map[string][]string{dbStandby: {"203.0.113.11"}})), false)
	requireControlPlane(t, "http://203.0.113.11:6432/", true)
	requireControlPlane(t, "http://127.0.0.1:6432/", true)
	requireControlPlane(t, "http://127.0.0.1:5432/", true) // the socket's port
	requireControlPlane(t, "http://203.0.113.11:5432/", false)
}
