package db

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // git blob ids are SHA-1; this is identification of upstream files, not security
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// The migration files are immutable once released: their sha256 is stored in every ledger, so a
// changed byte is a refusal on every database that applied the file. This test pins every embedded
// file (docs/phase-d2-stage2-plan.md 3.4, AC20). A new migration is added to migrationPins in the
// same commit that adds the file; changing a pin of a merged file is a change of a published
// migration and needs a new migration instead.
//
// downgradeSafe is "yes" or "no": whether a release older than the one that applied the migration
// still runs on the migrated database. A fork file also states it in its first line
// (`-- downgrade-safe: yes|no: <reason>`); upstream files must stay byte-identical, so for them the
// pin is the only place.
//
// blob is the git blob id of an upstream file, as `git hash-object` computes it. Files numbered
// below 5000 are upstream's and must carry one; fork files (5000-9999) must not.

type migrationPin struct {
	name          string
	sha256        string
	downgradeSafe string
	blob          string
}

// migrationUpstreamCommit is the upstream commit the blob ids were taken from
// (git ls-tree upstream/main db/migrations/); scripts and docs refer to it.
const migrationUpstreamCommit = "278de8b2e09a3702f77bbbd065ef35e4b7fdb1d0"

var migrationPins = []migrationPin{
	{"0001_llm_records.sql", "202d1b5f84f8091f5e676526b15fb66c888f39f73ddab60f46c251f19e19aa1e", "yes", "5b401d456857f707fbc25214ec9b4e5bc4b30157"},
	{"0002_llm_usage.sql", "3b3aece1c4c80bceef74d15652e1c97c972275d7c738a41938945f00588c8d39", "yes", "4fe0538e72a9780327bc7db1c4e14cecc97df49d"},
	{"0003_task_scope_icp_index.sql", "318e3e91b55552ed3322cb7fdb515133dc279d6fd76e5d94498bf56de1692331", "yes", "d30d05ae9bf5039bd28fcc581774dfa27b524dc7"},
	{"0004_llm_usage_ts_index.sql", "1f27b2411ebb064b7b7cd4d0d8fdfc2021ad74082019e6c72e772f706a00d028", "yes", "f9c66d0019f2b3a9b991628b830b33cb49c3082b"},
	{"5001_llm_usage_fork_columns.sql", "d55ce9857b9ef9d526ecaeeeeada7750f156761d1ec6878e154c198c7c81553f", "yes", ""},
	{"5002_tool_rag_observations.sql", "9fabb12a79280db7a102c57538bce03513feb0baa223d1d2771c28bca993a67c", "yes", ""},
	{"5003_finding_detections.sql", "c1587e96ca5d51ab22d013737aa48942d06e7fe7bb2c7e4e3005716227748ef7", "yes", ""},
}

var migrationDowngradeRE = regexp.MustCompile(`^-- downgrade-safe: (yes|no): \S.*$`)

func migrationDowngradeFlag(content []byte) (string, bool) {
	first, _, _ := bytes.Cut(content, []byte("\n"))
	m := migrationDowngradeRE.FindSubmatch(first)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

func migrationGitBlobID(content []byte) string {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// verifyMigrationPins compares the .sql files of fsys with pins and returns every difference.
func verifyMigrationPins(fsys fs.FS, pins []migrationPin) []string {
	var out []string
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return []string{"read migrations: " + err.Error()}
	}
	pinned := map[string]migrationPin{}
	for _, p := range pins {
		pinned[p.name] = p
	}
	present := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		present[name] = true
		p, ok := pinned[name]
		if !ok {
			out = append(out, name+": not pinned (add it to migrationPins)")
			continue
		}
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			out = append(out, name+": "+err.Error())
			continue
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != p.sha256 {
			out = append(out, fmt.Sprintf("%s: sha256 %s, pinned %s", name, got, p.sha256))
		}
		if bytes.IndexByte(content, '\r') >= 0 {
			out = append(out, name+": contains a CR byte")
		}
		if p.downgradeSafe != "yes" && p.downgradeSafe != "no" {
			out = append(out, name+": pinned downgrade-safe flag must be yes or no")
		}
		m := migrationNameRE.FindStringSubmatch(name)
		if m == nil {
			out = append(out, name+": not a migration file name")
			continue
		}
		version := 0
		fmt.Sscanf(m[1], "%d", &version)
		if version < 5000 {
			if p.blob == "" {
				out = append(out, name+": an upstream-range file (below 5000) needs a blob id")
			} else if got := migrationGitBlobID(content); got != p.blob {
				out = append(out, fmt.Sprintf("%s: git blob id %s, pinned %s (an upstream file must stay byte-identical)", name, got, p.blob))
			}
		} else {
			if p.blob != "" {
				out = append(out, name+": a fork file (5000 and above) must not carry an upstream blob id")
			}
			flag, ok := migrationDowngradeFlag(content)
			switch {
			case !ok:
				out = append(out, name+": the first line must be '-- downgrade-safe: yes|no: <reason>'")
			case flag != p.downgradeSafe:
				out = append(out, fmt.Sprintf("%s: header says downgrade-safe %s, pinned %s", name, flag, p.downgradeSafe))
			}
		}
	}
	for _, p := range pins {
		if !present[p.name] {
			out = append(out, p.name+": pinned but missing")
		}
	}
	return out
}

func TestMigrationPins(t *testing.T) {
	if diffs := verifyMigrationPins(embeddedMigrations(), migrationPins); len(diffs) > 0 {
		t.Errorf("the embedded migrations differ from their pins:\n  %s", strings.Join(diffs, "\n  "))
	}
}

// One changed byte in any file, a CR byte, a missing blob id or a missing file turns the check red.
func TestMigrationPinsDetectChanges(t *testing.T) {
	load := func() fstest.MapFS {
		out := fstest.MapFS{}
		entries, err := fs.ReadDir(embeddedMigrations(), ".")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			out[e.Name()] = &fstest.MapFile{Data: mustReadEmbedded(t, e.Name())}
		}
		return out
	}
	if diffs := verifyMigrationPins(load(), migrationPins); len(diffs) > 0 {
		t.Fatalf("the unchanged set must verify: %v", diffs)
	}
	for _, p := range migrationPins {
		changed := load()
		data := append([]byte(nil), changed[p.name].Data...)
		data[len(data)-1] ^= 0x01
		changed[p.name] = &fstest.MapFile{Data: data}
		if diffs := verifyMigrationPins(changed, migrationPins); len(diffs) == 0 {
			t.Errorf("%s: one changed byte was not detected", p.name)
		}

		crlf := load()
		crlf[p.name] = &fstest.MapFile{Data: bytes.ReplaceAll(crlf[p.name].Data, []byte("\n"), []byte("\r\n"))}
		if diffs := verifyMigrationPins(crlf, migrationPins); len(diffs) == 0 {
			t.Errorf("%s: CRLF line endings were not detected", p.name)
		}

		missing := load()
		delete(missing, p.name)
		if diffs := verifyMigrationPins(missing, migrationPins); len(diffs) == 0 {
			t.Errorf("%s: a missing file was not detected", p.name)
		}
	}
	extra := load()
	extra["5002_unpinned.sql"] = &fstest.MapFile{Data: []byte("-- downgrade-safe: yes: test\nSELECT 1;\n")}
	if diffs := verifyMigrationPins(extra, migrationPins); len(diffs) == 0 {
		t.Error("an unpinned file was not detected")
	}

	noBlob := append([]migrationPin(nil), migrationPins...)
	noBlob[0].blob = ""
	if diffs := verifyMigrationPins(load(), noBlob); len(diffs) == 0 {
		t.Error("an upstream-range file without a blob id was not detected")
	}
	forkWithBlob := append([]migrationPin(nil), migrationPins...)
	forkWithBlob[4].blob = migrationPins[0].blob
	if diffs := verifyMigrationPins(load(), forkWithBlob); len(diffs) == 0 {
		t.Error("a fork file with a blob id was not detected")
	}
	flipped := append([]migrationPin(nil), migrationPins...)
	flipped[4].downgradeSafe = "no"
	if diffs := verifyMigrationPins(load(), flipped); len(diffs) == 0 {
		t.Error("a downgrade-safe flag that differs from the header was not detected")
	}
	forkWithoutHeader := load()
	forkWithoutHeader["5001_llm_usage_fork_columns.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;\n")}
	if diffs := verifyMigrationPins(forkWithoutHeader, migrationPins); len(diffs) == 0 {
		t.Error("a fork file without the downgrade-safe header was not detected")
	}
}

// The baseline set is part of the format: a database adopted by this binary holds exactly these four
// rows, and the guarded cleanup recipe in the runbook compares against them.
func TestBaselineSetIsFrozen(t *testing.T) {
	want := []int{1, 2, 3, 5001}
	if !sameInts(baselineVersions, want) {
		t.Fatalf("baselineVersions = %v, want %v", baselineVersions, want)
	}
	ms, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	have := map[int]bool{}
	for _, m := range ms {
		have[m.version] = true
	}
	for _, v := range want {
		if !have[v] {
			t.Errorf("baseline version %04d is not embedded", v)
		}
	}
}

func TestMigrationsAreNotConvertedByGit(t *testing.T) {
	b, err := os.ReadFile("../.gitattributes")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		found = found || strings.TrimSpace(line) == "db/migrations/*.sql -text"
	}
	if !found {
		t.Error(".gitattributes has no 'db/migrations/*.sql -text' rule: a CRLF checkout would change the checksums")
	}
}
