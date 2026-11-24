package dbtest

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestMain gives this package its own database namespace (_dbtest) and reports a
// temporary database that a test left behind.
func TestMain(m *testing.M) {
	MustIsolateFromConfigFile()
	SuffixEnv("dbtest")
	code := m.Run()
	if err := CheckNoLeftovers(); err != nil {
		os.Stderr.WriteString("dbtest: " + err.Error() + "\n")
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

const miniDDL = `
CREATE TABLE parent (id BIGSERIAL PRIMARY KEY, note TEXT NOT NULL DEFAULT '');
CREATE TABLE child (
    id BIGSERIAL PRIMARY KEY,
    parent_id BIGINT NOT NULL REFERENCES parent(id),
    kind TEXT NOT NULL,
    CONSTRAINT ck_child_kind CHECK (kind IN ('a', 'b'))
);
CREATE INDEX idx_child_kind ON child(kind);
CREATE FUNCTION child_touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER trg_child_touch BEFORE INSERT ON child FOR EACH ROW EXECUTE FUNCTION child_touch();
`

func miniDB(t *testing.T) (*Temp, Queryer) {
	t.Helper()
	srv := RequireServer(t)
	tmp := TempDatabase(t, srv, "mini")
	tmp.Load(t, "mini schema", miniDDL)
	return tmp, tmp.Conn(t)
}

func lineSet(entries []Entry) map[string]bool {
	m := map[string]bool{}
	for _, e := range entries {
		m[e.Kind+" "+e.Name] = true
	}
	return m
}

func TestUpgradeCatalogListsEveryObjectKind(t *testing.T) {
	tmp, conn := miniDB(t)
	got := lineSet(tmp.Catalog(t, conn))
	for _, want := range []string{
		"table parent", "table child", "column child.kind", "column parent.id",
		"constraint child.ck_child_kind", "constraint child.child_pkey", "constraint child.child_parent_id_fkey",
		"index idx_child_kind", "index child_pkey", "trigger child.trg_child_touch",
		"function child_touch()", "sequence child_id_seq", "sequence parent_id_seq",
	} {
		if !got[want] {
			t.Errorf("listing has no %q", want)
		}
	}
}

func TestUpgradeCatalogDetectsValidityStateAndOwnership(t *testing.T) {
	tmp, conn := miniDB(t)
	base := Lines(tmp.Catalog(t, conn))
	cases := []struct {
		name, stmt, wantDiff string
	}{
		{"new unvalidated constraint", `ALTER TABLE child ADD CONSTRAINT ck_nv CHECK (kind <> 'z') NOT VALID`, "unexpected constraint child.ck_nv"},
		{"constraint left unvalidated", `ALTER TABLE child DROP CONSTRAINT ck_child_kind; ALTER TABLE child ADD CONSTRAINT ck_child_kind CHECK (kind IN ('a', 'b')) NOT VALID`, "changed constraint child.ck_child_kind"},
		{"invalid index", `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_child_kind'::regclass`, "changed index idx_child_kind"},
		{"disabled trigger", `ALTER TABLE child DISABLE TRIGGER trg_child_touch`, "changed trigger child.trg_child_touch"},
		{"detached sequence", `ALTER SEQUENCE child_id_seq OWNED BY NONE`, "changed sequence child_id_seq"},
		{"column default", `ALTER TABLE parent ALTER COLUMN note SET DEFAULT 'x'`, "changed column parent.note"},
		{"collation", `ALTER TABLE parent ALTER COLUMN note TYPE TEXT COLLATE "C"`, "changed column parent.note"},
		{"unlogged table", `ALTER TABLE child SET UNLOGGED`, "changed table child"},
		{"extension", `CREATE EXTENSION IF NOT EXISTS pg_trgm`, "unexpected extension pg_trgm"},
		{"view", `CREATE VIEW v_parent AS SELECT id FROM parent`, "unexpected view v_parent"},
		{"materialized view", `CREATE MATERIALIZED VIEW v_parent AS SELECT id FROM parent`, "unexpected relation v_parent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := RequireServer(t)
			tmp := TempDatabase(t, srv, "mutated")
			tmp.Load(t, "mini schema", miniDDL)
			tmp.Load(t, "mutation", c.stmt)
			got := Lines(tmp.Catalog(t, tmp.Conn(t)))
			diff := DiffLines(base, got)
			if !contains(diff, c.wantDiff) {
				t.Fatalf("difference %q not reported; got %v", c.wantDiff, diff)
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

func TestUpgradeCatalogIsStableAcrossRuns(t *testing.T) {
	tmp, conn := miniDB(t)
	first := Lines(tmp.Catalog(t, conn))
	second := Lines(tmp.Catalog(t, conn))
	if d := DiffLines(first, second); len(d) != 0 {
		t.Fatalf("two listings of one database differ: %v", d)
	}
}

func TestUpgradeDiffLinesNamesObjectsOnly(t *testing.T) {
	a := Entry{"table", "t1", "def-secret-a"}
	b := Entry{"table", "t1", "def-secret-b"}
	diff := DiffLines(Lines([]Entry{a}), Lines([]Entry{b, {"index", "i1", "x"}}))
	if len(diff) != 2 || diff[0] != "changed table t1" || diff[1] != "unexpected index i1" {
		t.Fatalf("diff = %v", diff)
	}
	for _, d := range diff {
		if strings.Contains(d, "secret") || strings.Contains(d, a.Hash()) {
			t.Fatalf("diff leaks a definition or hash: %q", d)
		}
	}
}

func TestUpgradeReadReferenceRejectsMalformedLines(t *testing.T) {
	if _, err := ReadReference(strings.NewReader("table t1 notahash\n")); err == nil {
		t.Fatal("a line without a sha256 was accepted")
	}
	lines, err := ReadReference(strings.NewReader("# header\n\n" + Entry{"table", "t1", "d"}.Line() + "\r\n"))
	if err != nil || len(lines) != 1 {
		t.Fatalf("lines=%v err=%v", lines, err)
	}
}

func TestUpgradeDigestsSeeRowsSequencesAndStayContentFree(t *testing.T) {
	tmp, conn := miniDB(t)
	ctx := context.Background()
	tmp.Load(t, "rows", `INSERT INTO parent(id, note) VALUES (1, '`+RowCanary+`-one'), (2, 'two');
		SELECT setval(pg_get_serial_sequence('parent', 'id'), 2);`)
	before, err := TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if before.Tables["parent"].Rows != 2 || before.Tables["child"].Rows != 0 || before.Sequences["parent_id_seq"] != 2 {
		t.Fatalf("unexpected digests: %+v", before)
	}
	// Change one value, delete a row, lower the sequence.
	tmp.Load(t, "edit", `UPDATE parent SET note = '`+RowCanary+`-changed' WHERE id = 1; DELETE FROM parent WHERE id = 2;
		SELECT setval(pg_get_serial_sequence('parent', 'id'), 1);`)
	after, err := TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	diff := DiffDigests(before, after)
	want := []string{
		"table parent: rows 2 -> 1",
		"table parent: column note values changed",
		"sequence parent_id_seq: last_value fell 2 -> 1",
	}
	for _, w := range want {
		if !contains(diff, w) {
			t.Errorf("difference %q not reported; got %v", w, diff)
		}
	}
	if d := DiffDigests(after, after); len(d) != 0 {
		t.Errorf("a state differs from itself: %v", d)
	}
	AssertNoSecret(t, "digest differences", strings.Join(diff, "\n"))
}

func TestUpgradeDigestDistinguishesNullFromTheText(t *testing.T) {
	tmp, conn := miniDB(t)
	ctx := context.Background()
	tmp.Load(t, "null", `INSERT INTO parent(id, note) VALUES (1, 'N')`)
	a, err := TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	tmp.Load(t, "null", `ALTER TABLE parent ALTER COLUMN note DROP NOT NULL; UPDATE parent SET note = NULL`)
	b, err := TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if d := DiffDigests(a, b); !contains(d, "table parent: column note values changed") {
		t.Fatalf("NULL and 'N' have one digest: %v", d)
	}
}

// AC16: with ARTEX_PG_DSN unset a database test of this package fails; it neither
// skips nor reads the config file (the marker listener is never dialled).
func TestUpgradeTestsFailWithoutEnvironmentVariable(t *testing.T) {
	res := RunChild(t, "TestUpgradeCatalogListsEveryObjectKind")
	if res.Dials != 0 {
		t.Fatalf("the test binary connected %d time(s) to the database named in the config file\n%s", res.Dials, res.Output)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Output, "--- FAIL: TestUpgradeCatalogListsEveryObjectKind") ||
		!strings.Contains(res.Output, EnvDSN+" is not set") {
		t.Fatalf("want the test to fail naming %s (exit %d)\n%s", EnvDSN, res.ExitCode, res.Output)
	}
}

// AC11: a temporary database that a test leaves behind is reported by the check the
// package TestMain runs after the tests.
func TestUpgradeLeftoverTemporaryDatabaseIsReported(t *testing.T) {
	srv := RequireServer(t)
	ctx := context.Background()
	name := srv.TempName("leftover")
	if err := srv.DropDatabase(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := CheckNoLeftovers(); err != nil {
		t.Fatalf("a clean server was reported: %v", err)
	}
	if err := srv.CreateDatabase(ctx, name); err != nil {
		t.Fatal(err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		if err := srv.DropDatabase(ctx, name); err != nil {
			t.Errorf("drop: %v", err)
		}
	}
	t.Cleanup(drop)
	err := CheckNoLeftovers()
	if err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("leftover %s not reported: %v", name, err)
	}
	drop()
	if err := CheckNoLeftovers(); err != nil {
		t.Fatalf("still reported after the drop: %v", err)
	}
}

// A view is listed by its definition and its columns: a changed definition and a changed column are both
// reported, an unchanged view is not.
func TestCatalogListsViewsByDefinitionAndColumns(t *testing.T) {
	srv := RequireServer(t)
	tmp := TempDatabase(t, srv, "viewdef")
	tmp.Load(t, "mini schema", miniDDL)
	tmp.Load(t, "view", `CREATE VIEW v_parent AS SELECT id FROM parent`)
	conn := tmp.Conn(t)
	base := Lines(tmp.Catalog(t, conn))
	if n := Count(tmp.Catalog(t, conn), "view"); n != 1 {
		t.Fatalf("the catalog lists %d views, want 1", n)
	}
	if diff := DiffLines(base, Lines(tmp.Catalog(t, conn))); len(diff) != 0 {
		t.Fatalf("an unchanged view differs: %v", diff)
	}
	for _, c := range []struct{ name, stmt string }{
		{"definition", `CREATE OR REPLACE VIEW v_parent AS SELECT id FROM parent WHERE id > 0`},
		{"columns", `CREATE OR REPLACE VIEW v_parent AS SELECT id, id AS id2 FROM parent`},
	} {
		tmp.Load(t, c.name, c.stmt)
		if diff := DiffLines(base, Lines(tmp.Catalog(t, conn))); !contains(diff, "changed view v_parent") {
			t.Errorf("a changed view %s was not reported: %v", c.name, diff)
		}
	}
}
