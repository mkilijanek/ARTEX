package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// AC1: provenance of the recorded schemas. The commit ids and sha256 values below
// are constants in test source as well as entries of testdata/upgrade/manifest.json;
// neither may change without the other.

const upgradeDir = "testdata/upgrade"

type releaseCommit struct{ tag, commit string }

// shapeMigration is one row of the ledger of a recorded shape: a release that ran the migration runner.
type shapeMigration struct {
	version      int
	name, sha256 string
}

type recordedShape struct {
	shape    string
	file     string
	sha256   string
	goddl    string // historical Go DDL that the releases of the shape ran after schema.sql; "" for a release with the runner
	goddlSHA string
	releases []releaseCommit
	// migrations are the ledger rows of a release that ran the migration runner (empty for the others): the
	// shape is schema.sql, the files of these migrations and the ledger holding exactly these rows.
	migrations []shapeMigration
}

// recordedShapes are the schema.sql files of the releases the upgrade tests start from
// (docs/phase-d2-plan.md section 2.4). Releases older than 0.5.9m are not recorded.
var recordedShapes = []recordedShape{
	{"r0509", "schema-b4b9600f5c8f.sql", "b4b9600f5c8f41cd13f0a33646fc2cb18fdc861a394c1167a1eb50a51517e4c6", "goddl-4bfc9dbe09c3.sql", "4bfc9dbe09c30629946c5b1918d67f164de08e9dd7b9857f8aeb95b9ad875624", []releaseCommit{
		{"v0.5.9m", "fd95c24b1889de8c25cad12d1c6355ac3fcdc594"},
		{"v0.5.10m", "43208e5354858a241e8410998c262c2ff733f472"},
		{"v0.5.11m", "58470d7581299b459be36be95ba93cf9c1598384"},
	}, nil},
	{"r0512", "schema-7996d35963a0.sql", "7996d35963a0eb882e339ea410058452ba9aadc653399edaa3b51858c4fec1f7", "goddl-4bfc9dbe09c3.sql", "4bfc9dbe09c30629946c5b1918d67f164de08e9dd7b9857f8aeb95b9ad875624", []releaseCommit{
		{"v0.5.12m", "450c877f91024fecd2f2023363db1d44cafd7fbc"},
	}, nil},
	// r0513 is the last release before the migration runner: its db/schema.sql is the schema that is
	// now frozen (docs/phase-d2-stage2-plan.md 3.1), and it ran the same Go DDL as the older releases.
	{"r0513", "schema-951f3d0c8859.sql", "951f3d0c88597f494b5fc1a87ca6694daa9d35e490d3f131d4fb36a2f49f963a", "goddl-4bfc9dbe09c3.sql", "4bfc9dbe09c30629946c5b1918d67f164de08e9dd7b9857f8aeb95b9ad875624", []releaseCommit{
		{"v0.5.13m", "b160905d0d17ec398296609a0d39263f5b72f761"},
	}, nil},
	// s2a is the first release with the migration ledger (0.5.15m, the migration runner): the same schema.sql
	// bytes as r0513 (frozen), no Go DDL, and the llm tables and the ledger of the four baseline migrations. It is
	// the database that the release with migration 0004 starts on (docs/phase-d2-stage2-plan.md 5, AC29).
	{"s2a", "schema-951f3d0c8859.sql", "951f3d0c88597f494b5fc1a87ca6694daa9d35e490d3f131d4fb36a2f49f963a", "", "", []releaseCommit{
		{"v0.5.15m", "ee60bbef5bfe263c1fdbef73dcc5f74fde8ddf75"},
	}, []shapeMigration{
		{1, "0001_llm_records.sql", "202d1b5f84f8091f5e676526b15fb66c888f39f73ddab60f46c251f19e19aa1e"},
		{2, "0002_llm_usage.sql", "3b3aece1c4c80bceef74d15652e1c97c972275d7c738a41938945f00588c8d39"},
		{3, "0003_task_scope_icp_index.sql", "318e3e91b55552ed3322cb7fdb515133dc279d6fd76e5d94498bf56de1692331"},
		{5001, "5001_llm_usage_fork_columns.sql", "d55ce9857b9ef9d526ecaeeeeada7750f156761d1ec6878e154c198c7c81553f"},
	}},
	// s3a is release 0.5.17m, the last one before migration 5003 (docs/detector-plan.md section 5): the same
	// schema.sql bytes (frozen), and the ledger of 0.5.15m plus 0004 and 5002. It is the database that the release
	// with the detection tables starts on, and the one the drop recipes of 5003 and 5002 take it back to.
	{"s3a", "schema-951f3d0c8859.sql", "951f3d0c88597f494b5fc1a87ca6694daa9d35e490d3f131d4fb36a2f49f963a", "", "", []releaseCommit{
		{"v0.5.17m", "024abf634efb0f48f13493b40fb4185c9fa1fc49"},
	}, []shapeMigration{
		{1, "0001_llm_records.sql", "202d1b5f84f8091f5e676526b15fb66c888f39f73ddab60f46c251f19e19aa1e"},
		{2, "0002_llm_usage.sql", "3b3aece1c4c80bceef74d15652e1c97c972275d7c738a41938945f00588c8d39"},
		{3, "0003_task_scope_icp_index.sql", "318e3e91b55552ed3322cb7fdb515133dc279d6fd76e5d94498bf56de1692331"},
		{4, "0004_llm_usage_ts_index.sql", "1f27b2411ebb064b7b7cd4d0d8fdfc2021ad74082019e6c72e772f706a00d028"},
		{5001, "5001_llm_usage_fork_columns.sql", "d55ce9857b9ef9d526ecaeeeeada7750f156761d1ec6878e154c198c7c81553f"},
		{5002, "5002_tool_rag_observations.sql", "9fabb12a79280db7a102c57538bce03513feb0baa223d1d2771c28bca993a67c"},
	}},
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type manifestFile struct {
	Note   string `json:"note"`
	Shapes []struct {
		Shape       string `json:"shape"`
		Fixture     string `json:"fixture"`
		SHA256      string `json:"sha256"`
		GoDDL       string `json:"goddl_fixture"`
		GoDDLSHA256 string `json:"goddl_sha256"`
		Releases    []struct {
			Tag    string `json:"tag"`
			Commit string `json:"commit"`
		} `json:"releases"`
		Migrations []struct {
			Version int    `json:"version"`
			Name    string `json:"name"`
			SHA256  string `json:"sha256"`
		} `json:"migrations"`
	} `json:"shapes"`
	Current struct {
		SchemaSQLSHA256 string `json:"schema_sql_sha256"`
		Reference       string `json:"reference"`
		ReferenceSHA256 string `json:"reference_sha256"`
		Migrations      []struct {
			Version int    `json:"version"`
			Name    string `json:"name"`
			SHA256  string `json:"sha256"`
		} `json:"migrations"`
	} `json:"current"`
}

func readManifest(t *testing.T) manifestFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(upgradeDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifestFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	return m
}

func TestUpgradeFixtureChecksums(t *testing.T) {
	for _, s := range recordedShapes {
		b, err := os.ReadFile(filepath.Join(upgradeDir, s.file))
		if err != nil {
			t.Errorf("%s: %v", s.shape, err)
			continue
		}
		if got := sha256Hex(b); got != s.sha256 {
			t.Errorf("%s: %s has sha256 %s, want %s (re-extract it with: git show <commit>:db/schema.sql)", s.shape, s.file, got, s.sha256)
		}
		if want := "schema-" + s.sha256[:12] + ".sql"; s.file != want {
			t.Errorf("%s: file name %s does not carry the first 12 hex digits of its sha256 (%s)", s.shape, s.file, want)
		}
		if (s.goddl == "") != (len(s.migrations) > 0) {
			t.Errorf("%s: a shape has either a historical Go DDL fixture (a release before the runner) or a ledger of migrations (a release with it)", s.shape)
		}
		if s.goddl == "" {
			continue
		}
		g, err := os.ReadFile(filepath.Join(upgradeDir, s.goddl))
		if err != nil {
			t.Errorf("%s: %v", s.shape, err)
			continue
		}
		if got := sha256Hex(g); got != s.goddlSHA {
			t.Errorf("%s: %s has sha256 %s, want %s (the historical Go DDL fixture is never edited)", s.shape, s.goddl, got, s.goddlSHA)
		}
		if want := "goddl-" + s.goddlSHA[:12] + ".sql"; s.goddl != want {
			t.Errorf("%s: file name %s does not carry the first 12 hex digits of its sha256 (%s)", s.shape, s.goddl, want)
		}
	}
}

// The releases v0.5.13m, v0.5.15m and v0.5.17m (shapes r0513, s2a and s3a) are the ones whose schema.sql is the frozen
// db/schema.sql: while the schema is frozen, the newest recorded shape holds exactly its bytes. A lockstep edit of schema.sql
// (docs/phase-d2-stage2-plan.md 3.1) ends that equality on purpose: this test is then changed in the same
// commit, to compare with the shape of the release that carried the edit.
func TestUpgradeFrozenSchemaIsTheNewestRecordedShape(t *testing.T) {
	newest := recordedShapes[len(recordedShapes)-1]
	if newest.shape != "s3a" {
		t.Fatalf("the newest recorded shape is %s, want s3a", newest.shape)
	}
	if newest.sha256 != currentSchemaSQLSHA256 {
		t.Errorf("shape %s pins schema sha256 %s, the frozen schema.sql is pinned as %s", newest.shape, newest.sha256, currentSchemaSQLSHA256)
	}
	fixture, err := os.ReadFile(filepath.Join(upgradeDir, newest.file))
	if err != nil {
		t.Fatal(err)
	}
	if string(fixture) != schemaSQL {
		t.Errorf("%s differs from the embedded db/schema.sql, which must stay byte-identical to the release v0.5.13m while it is frozen", newest.file)
	}
}

func TestUpgradeManifestMatchesConstantsInTestSource(t *testing.T) {
	m := readManifest(t)
	if len(m.Shapes) != len(recordedShapes) {
		t.Fatalf("manifest lists %d shapes, test source %d", len(m.Shapes), len(recordedShapes))
	}
	for i, want := range recordedShapes {
		got := m.Shapes[i]
		if got.Shape != want.shape || got.Fixture != want.file || got.SHA256 != want.sha256 {
			t.Errorf("shape %s: manifest has %s %s %s", want.shape, got.Shape, got.Fixture, got.SHA256)
		}
		if got.GoDDL != want.goddl || got.GoDDLSHA256 != want.goddlSHA {
			t.Errorf("shape %s: manifest has goddl %s %s, test source %s %s", want.shape, got.GoDDL, got.GoDDLSHA256, want.goddl, want.goddlSHA)
		}
		if len(got.Migrations) != len(want.migrations) {
			t.Errorf("shape %s: manifest lists %d migrations, test source %d", want.shape, len(got.Migrations), len(want.migrations))
		} else {
			for j, m := range want.migrations {
				if g := got.Migrations[j]; g.Version != m.version || g.Name != m.name || g.SHA256 != m.sha256 {
					t.Errorf("shape %s migration %d: manifest has %d %s %s, test source %d %s %s", want.shape, j, g.Version, g.Name, g.SHA256, m.version, m.name, m.sha256)
				}
			}
		}
		if len(got.Releases) != len(want.releases) {
			t.Errorf("shape %s: manifest lists %d releases, test source %d", want.shape, len(got.Releases), len(want.releases))
			continue
		}
		for j, r := range want.releases {
			if got.Releases[j].Tag != r.tag || got.Releases[j].Commit != r.commit {
				t.Errorf("shape %s release %d: manifest has %s %s, test source %s %s", want.shape, j, got.Releases[j].Tag, got.Releases[j].Commit, r.tag, r.commit)
			}
			// Flag lists are recorded per tag (docs/phase-d2-plan.md section 5.1).
			if _, err := os.Stat(filepath.Join(upgradeDir, "flags-"+r.tag+".txt")); err != nil {
				t.Errorf("no flag list for %s: %v", r.tag, err)
			}
		}
	}
}

func TestUpgradeHashedFilesHaveNoCarriageReturn(t *testing.T) {
	files := []string{"schema.sql"}
	entries, err := os.ReadDir(upgradeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, filepath.Join(upgradeDir, e.Name()))
		}
	}
	if len(files) < 1+len(recordedShapes) {
		t.Fatalf("only %d files to check", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if i := bytes.IndexByte(b, '\r'); i >= 0 {
			t.Errorf("%s contains a carriage return at byte %d; its sha256 would depend on the checkout (check .gitattributes)", f, i)
		}
	}
}

func TestUpgradeGitattributesKeepsHashedFilesByteExact(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "-text" {
			have[f[0]] = true
		}
	}
	for _, pattern := range []string{"db/schema.sql", "db/testdata/upgrade/**"} {
		if !have[pattern] {
			t.Errorf(".gitattributes has no %q -text line: a checkout may convert line endings and change the hashes", pattern)
		}
	}
}

// TestGitHistoryFixtures compares each fixture byte for byte with
// `git show <commit>:db/schema.sql` for every release commit of its shape. It needs
// git history: it skips, with the reason, where there is no .git (a container fed
// with files only), and fails instead when UPGRADE_GATE_REQUIRE_GIT=1, which
// scripts/upgrade-gate.sh and hosted CI set. Its name does not start with
// TestUpgrade: the gate script counts and forbids skips for those tests.
func TestGitHistoryFixtures(t *testing.T) {
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		if os.Getenv("UPGRADE_GATE_REQUIRE_GIT") == "1" {
			t.Fatalf("UPGRADE_GATE_REQUIRE_GIT=1 but there is no git history here: %v", err)
		}
		t.Skipf("no git history here (git rev-parse failed: %v); compared by scripts/upgrade-gate.sh and hosted CI instead", err)
	}
	root := strings.TrimSpace(string(top))
	for _, s := range recordedShapes {
		want, err := os.ReadFile(filepath.Join(upgradeDir, s.file))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range s.releases {
			cmd := exec.Command("git", "-C", root, "show", r.commit+":db/schema.sql")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			got, err := cmd.Output()
			if err != nil {
				t.Errorf("git show %s:db/schema.sql (%s): %v: %s (a shallow clone lacks the commit)", r.commit, r.tag, err, strings.TrimSpace(stderr.String()))
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from git show %s:db/schema.sql (%s)", s.file, r.commit, r.tag)
			}
		}
	}
}
