package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// restoreFindingCommentsTx re-inserts the analyst comments of an archived task.
// It runs after the findings insert, in the restore transaction, so any error
// rolls the whole restore back. It is deliberately not part of the
// insertArchiveRows allowlist: it needs the checks below.
//
//  1. No key or no rows: nothing to do. Packages written before comments existed
//     restore zero comments.
//  2. Rows in a package older than format 3 are rejected, as for evidence bindings.
//  3. One set-based ownership check: every archived row must belong to a finding
//     of the task being restored. A single foreign row fails the whole restore.
//  4. Original comment ids are kept and the sequence is NOT advanced. That is
//     safe for same-instance restore, the only kind supported (findings are
//     restored under their original ids too): the ids were issued by this
//     database's sequence and cannot be issued again. A package from another
//     instance, or a tampered one, collides on the primary key and fails the
//     restore loudly instead of silently re-keying comments.
//  5. Every row is re-validated like a fresh write: the body must already be in
//     normal form (NormalizeFindingCommentBody returns it unchanged without
//     error), author kind and subject pass validateCommentAuthor, and no finding
//     may end up above MaxFindingCommentsPerFinding comments. The table's CHECK
//     constraints and the primary key remain as a second line (a duplicate id
//     inside one package fails on the primary key).
//
// json_populate_recordset bypasses column defaults and inserts NULL for absent
// keys, so defaults are applied by hand in normalizeArchivedCommentRows. Any
// column added to the table later needs its default added there as well, or
// packages written before that column existed will fail on a NOT NULL violation.
//
// Errors never carry a comment body: driver errors are reduced to SQLSTATE and
// constraint name (the driver detail of a CHECK violation quotes the whole row).
func restoreFindingCommentsTx(tx *sql.Tx, snapshot *TaskArchiveSnapshot) error {
	rows, err := decodeArchiveRows(snapshot.Tables["finding_comments"])
	if err != nil {
		return fmt.Errorf("decode archived comments: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	if snapshot.FormatVersion < 3 {
		return ErrTaskArchiveFormatMismatch
	}
	normalizeArchivedCommentRows(rows)
	if err := validateArchivedCommentRows(rows); err != nil {
		return err
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("encode archived comments: %w", err)
	}
	var foreign int
	if err := tx.QueryRow(`SELECT count(*) FROM json_populate_recordset(NULL::finding_comments,$1::json) c
WHERE NOT EXISTS (SELECT 1 FROM findings f WHERE f.id=c.finding_id AND f.task_id=$2)`, string(raw), snapshot.TaskID).Scan(&foreign); err != nil {
		return sanitizeCommentWriteErr("check archived comment ownership", err)
	}
	if foreign != 0 {
		return fmt.Errorf("archived comments reference findings outside the archived task: %d rows", foreign)
	}
	var overCap int
	if err := tx.QueryRow(`SELECT count(*) FROM (SELECT c.finding_id, count(*) AS n FROM json_populate_recordset(NULL::finding_comments,$1::json) c GROUP BY c.finding_id) g
WHERE g.n + (SELECT count(*) FROM finding_comments x WHERE x.finding_id=g.finding_id) > $2`, string(raw), MaxFindingCommentsPerFinding).Scan(&overCap); err != nil {
		return sanitizeCommentWriteErr("check archived comment cap", err)
	}
	if overCap != 0 {
		return fmt.Errorf("archived comments exceed the per-finding comment limit (%d) for %d findings", MaxFindingCommentsPerFinding, overCap)
	}
	res, err := tx.Exec(`INSERT INTO finding_comments SELECT * FROM json_populate_recordset(NULL::finding_comments,$1::json)`, string(raw))
	if err != nil {
		return sanitizeCommentWriteErr("insert archived comments", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != int64(len(rows)) {
		return fmt.Errorf("restored %d of %d archived comments (err=%v)", n, len(rows), err)
	}
	return nil
}

// normalizeArchivedCommentRows fills the defaults that json_populate_recordset
// would otherwise replace with NULL.
func normalizeArchivedCommentRows(rows []map[string]any) {
	for _, row := range rows {
		if row["author_kind"] == nil {
			row["author_kind"] = "operator"
		}
	}
}

// validateArchivedCommentRows applies the write-path rules to every archived
// row. Errors name the row index only, never the body.
func validateArchivedCommentRows(rows []map[string]any) error {
	for i, row := range rows {
		body, ok := row["body"].(string)
		if !ok {
			return fmt.Errorf("archived comment row %d: body is not a string", i)
		}
		norm, err := NormalizeFindingCommentBody(body)
		if err != nil {
			return fmt.Errorf("archived comment row %d: %w", i, err)
		}
		if norm != body {
			return fmt.Errorf("archived comment row %d: body is not in normal form", i)
		}
		kind, kok := row["author_kind"].(string)
		subject, sok := row["author_subject"].(string)
		if !kok || !sok {
			return fmt.Errorf("archived comment row %d: author kind or subject is not a string", i)
		}
		if err := validateCommentAuthor(kind, subject); err != nil {
			return fmt.Errorf("archived comment row %d: %w", i, err)
		}
	}
	return nil
}
