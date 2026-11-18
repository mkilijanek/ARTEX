package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestDatabaseEndpoint(t *testing.T) {
	both := []DBEndpoint{{dbPrimary, 5432}, {dbStandby, 6432}}
	for _, tc := range []struct {
		name string
		dsn  string
		want []DBEndpoint
	}{
		{"single host, default port", "postgres://artex:pw@" + dbPrimary + "/artex?sslmode=disable", []DBEndpoint{{dbPrimary, 5432}}},
		{"single host, explicit port", "postgres://artex:pw@" + dbPrimary + ":6543/artex", []DBEndpoint{{dbPrimary, 6543}}},
		{"single host in the query", "postgres://artex:pw@/artex?host=" + dbPrimary + "&port=6543", []DBEndpoint{{dbPrimary, 6543}}},
		{"ipv6 literal", "postgres://artex:pw@[::1]:6543/artex", []DBEndpoint{{"::1", 6543}}},
		{"url query lists", multiHostDSNs["url query lists"], both},
		{"url authority list", multiHostDSNs["url authority list"], both},
		{"keyword/value", multiHostDSNs["keyword/value"], both},
		{"sslmode=prefer fallback repeats a host", multiHostDSNs["fallback tls (prefer)"], both},
		{"one port for every host", "postgres://artex:pw@/artex?host=" + dbPrimary + "," + dbStandby + "&port=5433", []DBEndpoint{{dbPrimary, 5433}, {dbStandby, 5433}}},
		{"keyword/value, one port for every host", "host=" + dbPrimary + "," + dbStandby + " port=5433 user=artex dbname=artex", []DBEndpoint{{dbPrimary, 5433}, {dbStandby, 5433}}},
		{"same host on two ports", "postgres://artex:pw@" + dbPrimary + ":5432," + dbPrimary + ":6432/artex", []DBEndpoint{{dbPrimary, 5432}, {dbPrimary, 6432}}},
		{"unix socket only", "postgres://artex:pw@/artex?host=/var/run/postgresql", []DBEndpoint{{"/var/run/postgresql", 5432}}},
		{"unix socket among hosts", "postgres://artex:pw@/artex?host=/var/run/postgresql," + dbStandby + "&port=5432,6432", []DBEndpoint{{"/var/run/postgresql", 5432}, {dbStandby, 6432}}},
		{"unix socket, keyword/value", "host=/tmp user=artex dbname=artex", []DBEndpoint{{"/tmp", 5432}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearPGEnv(t)
			got, err := DatabaseEndpoint(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DatabaseEndpoint = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("an unparsable dsn errors without echoing it", func(t *testing.T) {
		clearPGEnv(t)
		for _, dsn := range []string{"postgres://artex:s3cr3t@db-secret.example:notaport/artex", "host=db-secret.example port=abc password=s3cr3t"} {
			_, err := DatabaseEndpoint(dsn)
			if err == nil {
				t.Fatalf("%q parsed", dsn)
			}
			for _, bad := range []string{"s3cr3t", "db-secret", "notaport", "abc"} {
				if strings.Contains(err.Error(), bad) {
					t.Fatalf("error carries %q: %v", bad, err)
				}
			}
		}
	})
}

// The captured endpoints replace the DSN: after the scrub the environment holds nothing,
// and a socket-only capture does not fall back to reading a DSN again.
func TestWebFetchPolicyUsesCapturedDatabaseEndpoints(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("ARTEX_PG_DSN", "")
	res := newFakeResolver(map[string][]string{dbPrimary: {"203.0.113.10"}, dbStandby: {"203.0.113.11"}})
	cp := testControlPlane("1", res)
	cp.DBEndpoints, cp.DBCaptured = []DBEndpoint{{dbPrimary, 5432}, {dbStandby, 6432}}, true
	setTestTargetPolicy(t, staticRules(nil, nil), cp, false)
	for _, dest := range []string{"http://203.0.113.10:5432/", "http://203.0.113.11:6432/", "http://127.0.0.1:6432/", "http://" + dbStandby + ":6432/"} {
		requireControlPlane(t, dest, true)
	}
	requireControlPlane(t, "http://203.0.113.10:6432/", false)

	t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@203.0.113.10:5432/artex")
	cp.DBEndpoints, cp.DBCaptured = nil, true
	setTestTargetPolicy(t, staticRules(nil, nil), cp, false)
	requireControlPlane(t, "http://203.0.113.10:5432/", false) // captured and empty: the DSN is not read again
}
