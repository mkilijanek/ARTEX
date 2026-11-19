package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC10, invariant I7: db/schema.sql and the reference catalog cannot change without the recorded
// procedure. The sha256 values below are pinned in test source and in
// testdata/upgrade/manifest.json ("current"); the test needs no database, so it fails fast on a
// plain `go test ./db`. The DDL of the llm tables is protected by the migration pins
// (migrate_pins_test.go); the historical Go DDL of the old releases is pinned per recorded shape
// (TestUpgradeFixtureChecksums).
const (
	currentSchemaSQLSHA256 = "951f3d0c88597f494b5fc1a87ca6694daa9d35e490d3f131d4fb36a2f49f963a"
	currentReferenceSHA256 = "511b7806720b74403fdb24eacc7cb67e1827cad48875872465b1d9c627f9f21c"
)

const referenceFile = "reference-catalog-pg16.txt"

// schemaChangeProcedure is what a failing gate tells the developer to do.
const schemaChangeProcedure = `
db/schema.sql or the reference catalog changed. (The DDL of the llm tables lives in db/migrations/;
migrate_pins_test.go guards it, and a published migration file is never edited.)
Every schema change needs the recorded procedure (docs/schema-upgrade.md, "Changing the schema"):
  1. Record the previous release's db/schema.sql as a new fixture by commit id:
       git show <commit>:db/schema.sql > db/testdata/upgrade/schema-<sha12>.sql
     add it to recordedShapes in db/upgrade_fixture_test.go and to manifest.json. Never edit a fixture.
     Skip this when that release is already a recorded shape and the schema has not changed since it.
  2. Extend the data fixtures and the expected-changes file for the new shape (go test ./db -run
     '^TestUpgradeRowsPreserved' lists the differences that have no entry).
  3. Regenerate the reference catalog in a PostgreSQL 16 container:
       ARTEX_UPGRADE_REGENERATE=1 go test ./db -run TestRegenerateUpgradeReference
  4. Update the two sha256 constants in db/schema_gate_test.go and the "current" entry of manifest.json.
  5. A new NOT NULL column of an archived table needs an archiveRestoreDefaults entry and an
     archiveKnownUpgradeDiffs entry (db/upgrade_archive_test.go); re-check the file:line causes in the
     expected-changes file; raise MIN_UPGRADE_TESTS in scripts/upgrade-gate.sh if tests were added.`

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
	if len(failed) > 0 {
		t.Fatalf("schema change gate:\n  %s\n%s", strings.Join(failed, "\n  "), schemaChangeProcedure)
	}
}
