package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC10, invariant I7: db/schema.sql, the migrations and the reference catalog cannot change without the
// recorded procedure. The sha256 values below are pinned in test source and in
// testdata/upgrade/manifest.json ("current", which also lists the embedded migrations); the test needs no
// database, so it fails fast on a plain `go test ./db`. db/schema.sql is frozen at its 0.5.13m bytes. The
// DDL of the llm tables lives in the migrations, which migrate_pins_test.go pins file by file; the
// historical Go DDL of the old releases is pinned per recorded shape (TestUpgradeFixtureChecksums).
const (
	currentSchemaSQLSHA256 = "951f3d0c88597f494b5fc1a87ca6694daa9d35e490d3f131d4fb36a2f49f963a"
	currentReferenceSHA256 = "a0a549978b645735329a6b623ce1fc68d714ecb120c0a0cac168d1c987a48cd8"
)

const referenceFile = "reference-catalog-pg16.txt"

// schemaChangeProcedure is what a failing gate tells the developer to do.
const schemaChangeProcedure = `
db/schema.sql, a migration or the reference catalog changed. db/schema.sql is frozen at its 0.5.13m bytes:
every schema change is a numbered migration (docs/schema-upgrade.md, "Changing the schema"):
  1. Add db/migrations/5NNN_<slug>.sql with the next free number ON MAIN (never reserve numbers on a
     branch; renumber an unmerged file on a collision). First line: "-- downgrade-safe: yes|no: <reason>".
     No CONCURRENTLY, no VACUUM. A migration that is on main is never edited: a new one corrects it.
  2. Add the file to migrationPins (name, sha256, downgrade-safe) in db/migrate_pins_test.go and to
     "current.migrations" in manifest.json. Upstream files (0001-4999) also get their blob id.
  3. Only for an object that schema.sql re-creates on every start (the CHECK pairs, the triggers, the four
     functions, the data statements): edit that statement in schema.sql IN THE SAME COMMIT as the migration
     that changes it, and update the schema.sql sha256 (db/schema_gate_test.go, manifest.json) and the
     recorded r0513 comparison (TestUpgradeFrozenSchemaIsTheNewestRecordedShape).
  4. Record the previous release as a shape if its state is not recorded yet: its db/schema.sql as a fixture
     by commit id (git show <commit>:db/schema.sql > db/testdata/upgrade/schema-<sha12>.sql), its Go DDL
     fixture, its applied migrations; add it to recordedShapes in db/upgrade_fixture_test.go and to
     manifest.json. Never edit a fixture.
  5. Extend the data fixtures and expected-changes.json for each shape, the added schema_migrations rows
     included (go test ./db -run '^TestUpgradeRowsPreserved' lists the differences that have no entry).
  6. Regenerate the reference catalog in a PostgreSQL 16 container, review it as a diff, then update the
     reference sha256 (db/schema_gate_test.go, manifest.json "current"):
       ARTEX_UPGRADE_REGENERATE=1 go test ./db -run TestRegenerateUpgradeReference
  7. A new NOT NULL column of an archived table needs an archiveRestoreDefaults entry and an
     archiveKnownUpgradeDiffs entry (db/upgrade_archive_test.go); re-check the file:line causes in
     expected-changes.json; raise MIN_UPGRADE_TESTS in scripts/upgrade-gate.sh if tests were added.
  8. The CHANGELOG lists the migration with its downgrade-safe line.`

func TestUpgradeSchemaChangeGate(t *testing.T) {
	m := readManifest(t)
	type pin struct{ what, got, constant, manifest string }
	file, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(file) != schemaSQL {
		t.Fatal("db/schema.sql differs from the embedded copy: rebuild the test binary")
	}
	ref, err := os.ReadFile(filepath.Join(upgradeDir, referenceFile))
	if err != nil {
		t.Fatalf("%v\n%s", err, schemaChangeProcedure)
	}
	pins := []pin{
		{"db/schema.sql", sha256Hex(file), currentSchemaSQLSHA256, m.Current.SchemaSQLSHA256},
		{"reference catalog " + referenceFile, sha256Hex(ref), currentReferenceSHA256, m.Current.ReferenceSHA256},
	}
	var failed []string
	for _, p := range pins {
		if p.got != p.constant {
			failed = append(failed, p.what+" has sha256 "+p.got+", test source pins "+p.constant)
		}
		if p.constant != p.manifest {
			failed = append(failed, p.what+": test source pins "+p.constant+", manifest.json current has "+p.manifest)
		}
	}
	if m.Current.Reference != referenceFile {
		failed = append(failed, "manifest.json current names reference "+m.Current.Reference+", want "+referenceFile)
	}
	// The migrations as embedded, against the list of manifest.json "current".
	embedded, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int]string{}
	for _, l := range m.Current.Migrations {
		listed[l.Version] = l.Name + " " + l.SHA256
	}
	if len(m.Current.Migrations) != len(listed) {
		failed = append(failed, "manifest.json current.migrations lists a version twice")
	}
	for _, e := range embedded {
		want := e.name + " " + e.checksum
		switch got, ok := listed[e.version]; {
		case !ok:
			failed = append(failed, fmt.Sprintf("migration %04d (%s) is embedded and missing from manifest.json current.migrations", e.version, e.name))
		case got != want:
			failed = append(failed, fmt.Sprintf("migration %04d: manifest.json current.migrations has %q, the embedded file is %q", e.version, got, want))
		}
		delete(listed, e.version)
	}
	for version := range listed {
		failed = append(failed, fmt.Sprintf("manifest.json current.migrations lists version %04d, which is not embedded", version))
	}
	if len(failed) > 0 {
		t.Fatalf("schema change gate:\n  %s\n%s", strings.Join(failed, "\n  "), schemaChangeProcedure)
	}
}
