package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC10, invariant I7: db/schema.sql, the Go DDL constants and the reference catalog
// cannot change without the recorded procedure. The sha256 values below are pinned
// in test source and in testdata/upgrade/manifest.json ("current"); the test needs
// no database, so it fails fast on a plain `go test ./db`.
const (
	currentSchemaSQLSHA256 = "7996d35963a0eb882e339ea410058452ba9aadc653399edaa3b51858c4fec1f7"
	currentGoDDLSHA256     = "4bfc9dbe09c30629946c5b1918d67f164de08e9dd7b9857f8aeb95b9ad875624"
	currentReferenceSHA256 = "a74819449e16309f8d372c4b5697d76efb35c6a6b28fca60f3f32aca3d68fb87"
)

const referenceFile = "reference-catalog-pg16.txt"

// schemaChangeProcedure is what a failing gate tells the developer to do.
const schemaChangeProcedure = `
db/schema.sql, the Go DDL constants (llmRecordsSchema, llmRecordsMigrate, llmUsageSchema) or the reference
catalog changed. Every schema change needs the recorded procedure (docs/schema-upgrade.md, "Changing the schema"):
  1. Record the previous release's db/schema.sql as a new fixture by commit id:
       git show <commit>:db/schema.sql > db/testdata/upgrade/schema-<sha12>.sql
     add it to recordedShapes in db/upgrade_fixture_test.go and to manifest.json. Never edit a fixture.
  2. Extend the data fixtures and the expected-changes file for the new shape.
  3. Regenerate the goddl fixture and the reference catalog in a PostgreSQL 16 container:
       ARTEX_UPGRADE_REGENERATE=1 go test ./db -run TestRegenerateUpgradeReference
  4. Update the three sha256 constants in db/schema_gate_test.go and the "current" entry of manifest.json.`

// goDDLText is the Go DDL that startup runs after schema.sql (EnsureLLMRecordsTable,
// EnsureLLMUsageTable), as one text: the content of the goddl-<sha12>.sql fixture.
func goDDLText() string {
	return "-- llmRecordsSchema\n" + llmRecordsSchema +
		"-- llmRecordsMigrate\n" + llmRecordsMigrate +
		"-- llmUsageSchema\n" + llmUsageSchema
}

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
		{"Go DDL constants", sha256Hex([]byte(goDDLText())), currentGoDDLSHA256, m.Current.GoDDLSHA256},
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
	// The recorded Go DDL fixture is the text the constants produce.
	goddl, err := os.ReadFile(filepath.Join(upgradeDir, m.Current.GoDDLFixture))
	switch {
	case err != nil:
		failed = append(failed, "goddl fixture: "+err.Error())
	case string(goddl) != goDDLText():
		failed = append(failed, "fixture "+m.Current.GoDDLFixture+" differs from the Go DDL constants")
	case m.Current.GoDDLFixture != "goddl-"+currentGoDDLSHA256[:12]+".sql":
		failed = append(failed, "fixture name "+m.Current.GoDDLFixture+" does not carry the first 12 hex digits of the pinned sha256")
	}
	if m.Current.Reference != referenceFile {
		failed = append(failed, "manifest.json current names reference "+m.Current.Reference+", want "+referenceFile)
	}
	if len(failed) > 0 {
		t.Fatalf("schema change gate:\n  %s\n%s", strings.Join(failed, "\n  "), schemaChangeProcedure)
	}
}
