package db

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// AC23 (docs/phase-d2-stage2-plan.md 3.4): the tag-based immutability check of scripts/upgrade-gate.sh,
// tried on synthetic repositories. No release tag of the real history holds a migration file yet, so the
// real run compares nothing; this test is what shows that the check can fail. It needs git and bash, which
// the gate needs too: their absence is a failure, not a skip.

type gateRepo struct {
	t        *testing.T
	dir      string
	upstream string // the commit that plays the upstream commit: it holds 0001_up.sql only
}

func (r *gateRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	cmd.Env = gateGitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out))
}

func gateGitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=gate test", "GIT_AUTHOR_EMAIL=gate-test@example.invalid",
		"GIT_COMMITTER_NAME=gate test", "GIT_COMMITTER_EMAIL=gate-test@example.invalid")
}

func (r *gateRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *gateRepo) commit(message string) {
	r.t.Helper()
	r.git("add", "-A")
	r.git("-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
}

// newGateRepo makes the base history: the upstream commit (0001_up.sql), then a commit that adds the fork
// file 5001_fork.sql and carries the release tag v1.0.0, so the tag holds two migration files.
func newGateRepo(t *testing.T) *gateRepo {
	t.Helper()
	r := &gateRepo{t: t, dir: t.TempDir()}
	r.git("init", "-q")
	r.write("db/migrations/0001_up.sql", "SELECT 1;\n")
	r.commit("upstream")
	r.upstream = r.git("rev-parse", "HEAD")
	r.write("db/migrations/5001_fork.sql", "-- downgrade-safe: yes: test\nSELECT 2;\n")
	r.commit("release")
	r.git("tag", "v1.0.0")
	return r
}

type gateResult struct {
	code           int
	stdout, stderr string
}

// run starts the check; env entries replace the defaults of the same name.
func gateRun(t *testing.T, dir string, env ...string) gateResult {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "scripts", "upgrade-gate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, "--migration-tags", dir)
	cmd.Env = append(gateGitEnv(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := gateResult{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("the gate script did not run: %v", err)
		}
		res.code = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func (res gateResult) require(t *testing.T, code int, stdoutHas, stderrHas string) {
	t.Helper()
	if res.code != code {
		t.Errorf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, code, res.stdout, res.stderr)
	}
	if stdoutHas != "" && !strings.Contains(res.stdout, stdoutHas) {
		t.Errorf("stdout lacks %q:\n%s", stdoutHas, res.stdout)
	}
	if stderrHas != "" && !strings.Contains(res.stderr, stderrHas) {
		t.Errorf("stderr lacks %q:\n%s", stderrHas, res.stderr)
	}
}

func TestUpgradeGateMigrationTagCheck(t *testing.T) {
	for _, tool := range []string{"git", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed: the gate script needs it (%v)", tool, err)
		}
	}

	t.Run("unchanged history passes", func(t *testing.T) {
		r := newGateRepo(t)
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream).require(t, 0,
			"2 tagged file(s) in 1 tag(s) are identical at HEAD", "")
		res := gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream)
		res.require(t, 0, "migration upstream check: 1 upstream-range file(s) equal upstream commit", "")
	})

	t.Run("a new migration after the tag passes", func(t *testing.T) {
		r := newGateRepo(t)
		r.write("db/migrations/5002_next.sql", "-- downgrade-safe: yes: test\nSELECT 3;\n")
		r.commit("next migration")
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream).require(t, 0, "identical at HEAD", "")
	})

	t.Run("a tagged fork file edited at HEAD fails", func(t *testing.T) {
		r := newGateRepo(t)
		r.write("db/migrations/5001_fork.sql", "-- downgrade-safe: yes: test\nSELECT 2; -- edited\n")
		r.commit("edit a released migration")
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream).require(t, 1, "",
			"db/migrations/5001_fork.sql differs at HEAD from release tag v1.0.0")
	})

	t.Run("a tagged upstream file edited at HEAD fails both checks", func(t *testing.T) {
		r := newGateRepo(t)
		r.write("db/migrations/0001_up.sql", "SELECT 1; -- edited\n")
		r.commit("edit an upstream migration")
		res := gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream)
		res.require(t, 1, "", "db/migrations/0001_up.sql differs at HEAD from release tag v1.0.0")
		res.require(t, 1, "", "db/migrations/0001_up.sql differs from upstream commit")
	})

	t.Run("a tagged file deleted at HEAD fails", func(t *testing.T) {
		r := newGateRepo(t)
		r.git("rm", "-q", "db/migrations/5001_fork.sql")
		r.commit("delete a released migration")
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream).require(t, 1, "",
			"db/migrations/5001_fork.sql is in release tag v1.0.0 and is missing at HEAD")
	})

	t.Run("a tagged file renamed at HEAD fails", func(t *testing.T) {
		r := newGateRepo(t)
		r.git("mv", "db/migrations/5001_fork.sql", "db/migrations/5001_other.sql")
		r.commit("rename a released migration")
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream).require(t, 1, "", "5001_fork.sql is in release tag v1.0.0 and is missing at HEAD")
	})

	t.Run("a tag that is not a release tag is ignored", func(t *testing.T) {
		r := newGateRepo(t)
		r.git("tag", "draft-1")
		r.write("db/migrations/5001_fork.sql", "-- downgrade-safe: no: edited\nSELECT 4;\n")
		r.commit("edit after a draft tag")
		r.git("tag", "-d", "v1.0.0")
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream).require(t, 0, "no release tag holds a file of db/migrations yet", "")
	})

	t.Run("an upstream-range file that upstream does not have fails", func(t *testing.T) {
		r := newGateRepo(t)
		r.write("db/migrations/0002_mine.sql", "SELECT 5;\n")
		r.commit("a made-up upstream number")
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT="+r.upstream).require(t, 1, "identical at HEAD",
			"db/migrations/0002_mine.sql is numbered in the upstream range (below 5000) and upstream commit")
	})

	t.Run("an upstream commit that is not in the repository is NOT RUN", func(t *testing.T) {
		r := newGateRepo(t)
		gateRun(t, r.dir, "UPGRADE_GATE_UPSTREAM_COMMIT=0123456789abcdef0123456789abcdef01234567").require(t, 0,
			"migration upstream check: NOT RUN", "")
	})

	t.Run("without git history: NOT RUN, and a failure with UPGRADE_GATE_REQUIRE_GIT=1", func(t *testing.T) {
		dir := t.TempDir()
		ceiling := "GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)
		gateRun(t, dir, ceiling).require(t, 0, "migration tag check: NOT RUN (no git history here", "")
		gateRun(t, dir, ceiling, "UPGRADE_GATE_REQUIRE_GIT=1").require(t, 1, "", "UPGRADE_GATE_REQUIRE_GIT=1 but there is no git history here")
	})
}
