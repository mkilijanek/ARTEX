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

type recordedShape struct {
	shape    string
	file     string
	sha256   string
	goddl    string // historical Go DDL that the releases of the shape ran after schema.sql
	goddlSHA string
	releases []releaseCommit
}

// recordedShapes are the schema.sql files of the releases the upgrade tests start from
// (docs/phase-d2-plan.md section 2.4). Releases older than 0.5.9m are not recorded.
var recordedShapes = []recordedShape{
	{"r0509", "schema-b4b9600f5c8f.sql", "b4b9600f5c8f41cd13f0a33646fc2cb18fdc861a394c1167a1eb50a51517e4c6", "goddl-4bfc9dbe09c3.sql", "4bfc9dbe09c30629946c5b1918d67f164de08e9dd7b9857f8aeb95b9ad875624", []releaseCommit{
		{"v0.5.9m", "fd95c24b1889de8c25cad12d1c6355ac3fcdc594"},
		{"v0.5.10m", "43208e5354858a241e8410998c262c2ff733f472"},
		{"v0.5.11m", "58470d7581299b459be36be95ba93cf9c1598384"},
	}},
	{"r0512", "schema-7996d35963a0.sql", "7996d35963a0eb882e339ea410058452ba9aadc653399edaa3b51858c4fec1f7", "goddl-4bfc9dbe09c3.sql", "4bfc9dbe09c30629946c5b1918d67f164de08e9dd7b9857f8aeb95b9ad875624", []releaseCommit{
		{"v0.5.12m", "450c877f91024fecd2f2023363db1d44cafd7fbc"},
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
	} `json:"shapes"`
	Current struct {
		SchemaSQLSHA256 string `json:"schema_sql_sha256"`
		Reference       string `json:"reference"`
		ReferenceSHA256 string `json:"reference_sha256"`
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
