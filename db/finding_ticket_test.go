package db

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestFindingTicketIDColumnIsIdempotent covers a fresh start, an upgrade of a
// database that predates the column, and a second start: the column exists,
// every finding is empty after the upgrade and a stored value survives a restart.
func TestFindingTicketIDColumnIsIdempotent(t *testing.T) {
	// Own database: dropping the column must not affect any other test.
	base := testDSN(t)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "_ticket_upgrade"
	dsn := u.String()
	upgradeDB := strings.TrimPrefix(u.Path, "/")
	adminURL := *u
	adminURL.Path = "/postgres"
	t.Cleanup(func() {
		admin, err := sql.Open("pgx", adminURL.String())
		if err != nil {
			t.Errorf("drop %s: %v", upgradeDB, err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS "` + upgradeDB + `" WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", upgradeDB, err)
		}
	})
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Put the throwaway database in the pre-feature shape.
	if _, err := d.Exec(`ALTER TABLE findings DROP COLUMN IF EXISTS ticket_id`); err != nil {
		t.Fatal(err)
	}
	id, err := d.AddFinding(0, 0, "ticket-column", "legacy finding", "low", "s", "e", "w", nil)
	if err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(dsn)
	if err != nil {
		t.Fatalf("startup on a database without the column: %v", err)
	}
	defer upgraded.Close()
	var dataType, nullable, def string
	if err := upgraded.QueryRow(`SELECT data_type, is_nullable, COALESCE(column_default,'') FROM information_schema.columns
		WHERE table_name='findings' AND column_name='ticket_id' AND table_schema=current_schema()`).Scan(&dataType, &nullable, &def); err != nil {
		t.Fatalf("findings.ticket_id missing after startup: %v", err)
	}
	if dataType != "text" || nullable != "NO" || !strings.HasPrefix(def, "''") {
		t.Fatalf("ticket_id column shape: type=%s nullable=%s default=%s", dataType, nullable, def)
	}
	f, err := upgraded.GetFinding(id)
	if err != nil || f == nil || f.TicketID != "" {
		t.Fatalf("existing finding after upgrade: %+v err=%v", f, err)
	}

	if n, err := upgraded.SetFindingTicketID(id, "SEC-1234"); err != nil || n != 1 {
		t.Fatalf("SetFindingTicketID: n=%d err=%v", n, err)
	}
	again, err := Open(dsn)
	if err != nil {
		t.Fatalf("second startup: %v", err)
	}
	defer again.Close()
	if f, err = again.GetFinding(id); err != nil || f == nil || f.TicketID != "SEC-1234" {
		t.Fatalf("value after second startup: %+v err=%v", f, err)
	}
}

func TestNormalizeFindingTicketID(t *testing.T) {
	long := strings.Repeat("é", MaxFindingTicketIDRunes) // 128 code points, 256 bytes
	for _, tc := range []struct {
		name, in, want string
		ok             bool
	}{
		{"plain", "SEC-1234", "SEC-1234", true},
		{"empty clears", "", "", true},
		{"whitespace only clears", " \t ", "", true},
		{"trimmed", "  SEC-1  ", "SEC-1", true},
		{"inner space and unicode", "JIRA 12 / zażółć", "JIRA 12 / zażółć", true},
		{"url", "https://tracker.example/browse/SEC-1?a=b&c=d", "https://tracker.example/browse/SEC-1?a=b&c=d", true},
		{"exactly 128 code points", long, long, true},
		{"129 code points", long + "x", "", false},
		{"128 after trim", "  " + long + "  ", long, true},
		{"line feed inside", "SEC-1\nSEC-2", "", false},
		{"carriage return inside", "SEC-1\rSEC-2", "", false},
		{"tab inside", "SEC\t1", "", false},
		{"nul", "SEC\x001", "", false},
		{"escape", "SEC\x1b[31m", "", false},
		{"delete", "SEC\x7f", "", false},
		{"c1 control", "SEC\u00851", "", false},
		{"line separator", "SEC 1", "", false},
		{"paragraph separator", "SEC 1", "", false},
		{"invalid utf-8", "SEC\xff1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeFindingTicketID(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("NormalizeFindingTicketID(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestFindingTicketIDStorageAndReads: the setter touches only ticket_id (no node
// payload mirror, which agents read) and every read path returns the value.
func TestFindingTicketIDStorageAndReads(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tk, err := d.CreateTask("ticket id storage", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	nodeID, err := es.AddNode(KindFinding, map[string]any{"summary": "s", "severity": "high"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "__ticket_storage__"
	id, err := d.AddFinding(tk.ID, nodeID, marker, "named", "high", "summary", "poc", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteFinding(id)
	if _, err := d.SetFindingStatus(id, FindingConfirmed); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE findings SET report='# R' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var payloadBefore string
	if err := d.QueryRow(`SELECT payload::text FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&payloadBefore); err != nil {
		t.Fatal(err)
	}

	if n, err := d.SetFindingTicketID(id+1_000_000_000, "X-1"); err != nil || n != 0 {
		t.Fatalf("unknown finding: n=%d err=%v", n, err)
	}
	if n, err := d.SetFindingTicketID(id, "SEC-77"); err != nil || n != 1 {
		t.Fatalf("set: n=%d err=%v", n, err)
	}

	f, err := d.GetFinding(id)
	if err != nil || f == nil {
		t.Fatal(f, err)
	}
	if f.TicketID != "SEC-77" || f.Status != FindingConfirmed || f.Severity != "high" || f.Name != "named" ||
		f.VulnClass != marker || f.Evidence != "poc" || f.Report != "# R" {
		t.Fatalf("GetFinding: %+v", f)
	}
	page, _, err := d.ListFindingsPage(FindingFilter{VulnClass: marker}, 1, 10)
	if err != nil || len(page) != 1 || page[0].TicketID != "SEC-77" {
		t.Fatalf("ListFindingsPage: %+v err=%v", page, err)
	}
	all, err := d.ListFindings(500)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, x := range all {
		if x.ID == id {
			seen = x.TicketID == "SEC-77"
		}
	}
	if !seen {
		t.Fatal("ListFindings does not return the ticket id")
	}
	exp, err := d.ListFindingsForExport(FindingFilter{}, []int64{id})
	if err != nil || len(exp) != 1 || exp[0].TicketID != "SEC-77" {
		t.Fatalf("ListFindingsForExport: %+v err=%v", exp, err)
	}
	meta, err := d.FindingMetaByNodeID(tk.ID)
	if err != nil || meta[nodeID].TicketID != "SEC-77" {
		t.Fatalf("FindingMetaByNodeID: %+v err=%v", meta, err)
	}

	// The exploration node payload is what agents read: it must not carry the ticket id.
	var payloadAfter string
	if err := d.QueryRow(`SELECT payload::text FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&payloadAfter); err != nil {
		t.Fatal(err)
	}
	if payloadAfter != payloadBefore || strings.Contains(payloadAfter, "SEC-77") {
		t.Fatalf("node payload changed: %s -> %s", payloadBefore, payloadAfter)
	}

	// Clearing.
	if n, err := d.SetFindingTicketID(id, ""); err != nil || n != 1 {
		t.Fatalf("clear: n=%d err=%v", n, err)
	}
	if f, _ = d.GetFinding(id); f.TicketID != "" {
		t.Fatalf("not cleared: %q", f.TicketID)
	}
}

// The whole-struct JSON of a DBFinding is what the chat mention puts into an LLM
// prompt; the operator-only ticket id must never be part of it.
func TestDBFindingJSONOmitsTicketID(t *testing.T) {
	blob, err := json.Marshal(&DBFinding{ID: 1, TicketID: "SEC-SECRET-9"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "SEC-SECRET-9") || strings.Contains(strings.ToLower(string(blob)), "ticket") {
		t.Fatalf("DBFinding JSON exposes the ticket id: %s", blob)
	}
}

// The retest snapshot is read by the retester agent through get_finding_retest_context.
func TestFindingRetestSnapshotOmitsTicketID(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	id, err := d.AddFinding(0, 0, "ticket-snapshot", "n", "high", "s", "e", "w", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteFinding(id)
	const canary = "TICKETLEAK-31c2f9"
	if _, err := d.SetFindingTicketID(id, canary); err != nil {
		t.Fatal(err)
	}
	r, _, _, err := d.CreateFindingRetest(t.Context(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM finding_retests WHERE id=$1`, r.ID)
		d.Exec(`DELETE FROM conversations WHERE id=$1`, *r.ConversationID)
	})
	var snapshot string
	if err := d.QueryRow(`SELECT snapshot::text FROM finding_retests WHERE id=$1`, r.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot, `"vulnclass"`) {
		t.Fatalf("snapshot lost the finding: %s", snapshot)
	}
	if strings.Contains(snapshot, canary) || strings.Contains(snapshot, "ticket_id") {
		t.Fatalf("retest snapshot exposes the ticket id: %s", snapshot)
	}
	loaded, err := d.FindingRetestForConversation(t.Context(), *r.ConversationID)
	if err != nil || loaded == nil {
		t.Fatal(loaded, err)
	}
	if strings.Contains(string(loaded.Snapshot), canary) {
		t.Fatalf("agent-facing retest context exposes the ticket id: %s", loaded.Snapshot)
	}
}

// TestFindingTicketIDArchiveRoundTrip: the value survives archive + restore, and an
// archive written before the column existed (key absent, or null) restores empty.
func TestFindingTicketIDArchiveRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err = d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(row map[string]any)
		want   string
		fail   bool
	}{
		{"kept", func(map[string]any) {}, "SEC-1234", false},
		{"legacy archive without the key", func(row map[string]any) { delete(row, "ticket_id") }, "", false},
		{"legacy archive with null", func(row map[string]any) { row["ticket_id"] = nil }, "", false},
		{"padded value is stored trimmed", func(row map[string]any) { row["ticket_id"] = "  SEC-9  " }, "SEC-9", false},
		{"over-long", func(row map[string]any) { row["ticket_id"] = strings.Repeat("x", MaxFindingTicketIDRunes+1) }, "", true},
		{"newline", func(row map[string]any) { row["ticket_id"] = "SEC-1\nSEC-2" }, "", true},
		{"control character", func(row map[string]any) { row["ticket_id"] = "SEC\x1b[31m" }, "", true},
		{"number", func(row map[string]any) { row["ticket_id"] = 42 }, "", true},
		{"object", func(row map[string]any) { row["ticket_id"] = map[string]any{"a": "b"} }, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, err := d.CreateTask("ticket archive", "fixture", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID); d.DeleteTask(task.ID) }()
			f, err := d.Exploration(task.ExplorationID).RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, Summary: "ticket", Severity: "low"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.SetFindingTicketID(f.FindingID, "SEC-1234"); err != nil {
				t.Fatal(err)
			}
			if err = d.SetPaused(task.ID, true); err != nil {
				t.Fatal(err)
			}
			archive, err := d.QueueTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := d.SnapshotTaskArchive(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/ticket-test.tar.zst", "fixture", 1, 1); err != nil {
				t.Fatal(err)
			}
			var rows []map[string]any
			if err = json.Unmarshal(snapshot.Tables["findings"], &rows); err != nil || len(rows) != 1 {
				t.Fatalf("findings table in the snapshot: %v %s", err, snapshot.Tables["findings"])
			}
			if rows[0]["ticket_id"] != "SEC-1234" {
				t.Fatalf("snapshot lacks the ticket id: %v", rows[0])
			}
			for _, row := range rows {
				tc.mutate(row)
			}
			snapshot.Tables["findings"], _ = json.Marshal(rows)
			// Change the live value so a restore that only left the row alone would be caught.
			if _, err = d.SetFindingTicketID(f.FindingID, "CHANGED-AFTER-ARCHIVE"); err != nil {
				t.Fatal(err)
			}
			if _, err = d.QueueTaskArchiveRestore(archive.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err = d.RestoreTaskArchive(archive.ID, snapshot, 0)
			if tc.fail {
				if err == nil {
					t.Fatal("restore accepted an invalid ticket_id")
				}
				if strings.Contains(err.Error(), "SEC") || !strings.Contains(err.Error(), "ticket_id") {
					t.Fatalf("restore error must name ticket_id and not echo the value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			got, err := d.GetFinding(f.FindingID)
			if err != nil || got == nil {
				t.Fatal(got, err)
			}
			if got.TicketID != tc.want {
				t.Fatalf("ticket id after restore = %q, want %q", got.TicketID, tc.want)
			}
		})
	}
}
