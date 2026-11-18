package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// Analyst comments on findings (issue #65). The table is deliberately separate
// from findings: nothing here is joined into DBFinding, so comment text cannot
// reach agent prompts, retest snapshots or exports.

const (
	// MaxFindingCommentRunes is the body limit in Unicode code points.
	MaxFindingCommentRunes = 10000
	// MaxFindingCommentsPerFinding bounds table growth per finding.
	MaxFindingCommentsPerFinding = 2000

	findingCommentDefaultLimit = 50
	findingCommentMaxLimit     = 200
	findingCommentMaxCursor    = 256
	findingCommentMaxSubject   = 200
)

var (
	// ErrFindingCommentInvalid wraps every validation failure. Its messages are
	// fixed per rule and never contain the rejected text.
	ErrFindingCommentInvalid = errors.New("invalid finding comment")
	// ErrFindingCommentCursor reports a malformed list cursor.
	ErrFindingCommentCursor = errors.New("invalid finding comment cursor")
	// ErrFindingCommentLimit reports that the per-finding cap is reached.
	ErrFindingCommentLimit = errors.New("comment limit reached for this finding")
)

// FindingComment is one stored comment. CreatedAt is in UTC.
type FindingComment struct {
	ID            int64
	FindingID     int64
	Body          string
	AuthorKind    string
	AuthorSubject string
	CreatedAt     time.Time
}

func invalidComment(rule string) error {
	return fmt.Errorf("%w: %s", ErrFindingCommentInvalid, rule)
}

// NormalizeFindingCommentBody applies the body rules: CRLF becomes LF, the text
// is trimmed, and empty, over-long, invalid UTF-8 and unsafe characters are
// rejected: C0 controls other than \n and \t, DEL, C1 controls U+0080-U+009F (incl.
// NEL U+0085, which is only trimmed at the edges); bidi controls U+061C, U+200E,
// U+200F, U+202A-202E, U+2066-2069; invisible characters U+200B, U+2060-U+2064,
// U+FEFF, U+FFF9-U+FFFB and tags U+E0000-U+E007F. Allowed on purpose: ZWNJ, ZWJ,
// variation selectors, soft hyphen, U+2028/U+2029.
// The same set is mirrored in web/src/lib/finding-comments.ts; change both together.
// Returned errors wrap ErrFindingCommentInvalid with a fixed message.
func NormalizeFindingCommentBody(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", invalidComment("body is not valid UTF-8")
	}
	body := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	if body == "" {
		return "", invalidComment("body must not be empty")
	}
	if utf8.RuneCountInString(body) > MaxFindingCommentRunes {
		return "", invalidComment("body is too long")
	}
	for _, r := range body {
		switch {
		case r < 0x20 && r != '\n' && r != '\t', r >= 0x7f && r <= 0x9f:
			return "", invalidComment("body contains control characters")
		case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return "", invalidComment("body contains bidirectional control characters")
		case r == 0x200b, r >= 0x2060 && r <= 0x2064, r == 0xfeff, r >= 0xfff9 && r <= 0xfffb, r >= 0xe0000 && r <= 0xe007f:
			return "", invalidComment("body contains zero-width characters")
		}
	}
	return body, nil
}

// sanitizeCommentWriteErr maps driver errors to errors that never carry the
// driver's detail text (a CHECK violation quotes the failing row, i.e. the body).
func sanitizeCommentWriteErr(op string, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("%s: %w", op, err)
	}
	switch pgErr.Code {
	case "23503": // finding deleted concurrently
		return ErrFindingNotFound
	case "23514":
		return fmt.Errorf("%w: constraint %s", ErrFindingCommentInvalid, pgErr.ConstraintName)
	}
	return fmt.Errorf("%s: sqlstate %s constraint %q", op, pgErr.Code, pgErr.ConstraintName)
}

// validateCommentAuthor is shared by AddFindingComment and the archive restore.
func validateCommentAuthor(authorKind, authorSubject string) error {
	if strings.TrimSpace(authorKind) == "" || len(authorKind) > findingCommentMaxSubject {
		return invalidComment("author kind is missing or too long")
	}
	if strings.TrimSpace(authorSubject) == "" || utf8.RuneCountInString(authorSubject) > findingCommentMaxSubject || !utf8.ValidString(authorSubject) {
		return invalidComment("author subject is missing or too long")
	}
	return nil
}

// AddFindingComment appends a comment. It runs in a plain transaction that takes
// LockFindingEvidenceTx (task row lock with the archive-state check, then the
// finding row), not WithEvidenceTx, so it never waits behind the process-wide
// evidence lock held across archive file copies. The finding row lock serialises
// inserts per finding: ids are allocated in commit order and the cap count is
// exact. Returns ErrFindingNotFound, ErrTaskArchiveState,
// ErrFindingCommentInvalid or ErrFindingCommentLimit. The finding is not modified.
func (d *DB) AddFindingComment(ctx context.Context, findingID int64, authorKind, authorSubject, body string) (*FindingComment, error) {
	if err := validateCommentAuthor(authorKind, authorSubject); err != nil {
		return nil, err
	}
	norm, err := NormalizeFindingCommentBody(body)
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin finding comment: %w", err)
	}
	defer tx.Rollback()
	if err := LockFindingEvidenceTx(tx, findingID, nil); err != nil {
		if errors.Is(err, ErrFindingNotFound) || errors.Is(err, ErrTaskArchiveState) {
			return nil, err
		}
		return nil, sanitizeCommentWriteErr("lock finding for comment", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM finding_comments WHERE finding_id=$1`, findingID).Scan(&n); err != nil {
		return nil, sanitizeCommentWriteErr("count finding comments", err)
	}
	if n >= MaxFindingCommentsPerFinding {
		return nil, ErrFindingCommentLimit
	}
	c := &FindingComment{FindingID: findingID, Body: norm, AuthorKind: authorKind, AuthorSubject: authorSubject}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO finding_comments (finding_id, body, author_kind, author_subject) VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
		findingID, norm, authorKind, authorSubject).Scan(&c.ID, &c.CreatedAt)
	if err != nil {
		return nil, sanitizeCommentWriteErr("insert finding comment", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, sanitizeCommentWriteErr("commit finding comment", err)
	}
	c.CreatedAt = c.CreatedAt.UTC()
	return c, nil
}

type findingCommentCursor struct {
	ID int64 `json:"id"`
}

func decodeFindingCommentCursor(cursor string) (int64, error) {
	if len(cursor) > findingCommentMaxCursor {
		return 0, ErrFindingCommentCursor
	}
	blob, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrFindingCommentCursor
	}
	var c findingCommentCursor
	dec := json.NewDecoder(strings.NewReader(string(blob)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.ID <= 0 || dec.More() {
		return 0, ErrFindingCommentCursor
	}
	return c.ID, nil
}

// ListFindingComments returns up to limit comments of one finding in id order
// (default 50, maximum 200) and the opaque cursor of the next page ("" on the
// last page). The cursor is a keyset on (finding_id, id); created_at is display
// data only. Plain read, no lock. A malformed cursor returns ErrFindingCommentCursor.
func (d *DB) ListFindingComments(ctx context.Context, findingID int64, cursor string, limit int) ([]*FindingComment, string, error) {
	if limit <= 0 {
		limit = findingCommentDefaultLimit
	}
	if limit > findingCommentMaxLimit {
		limit = findingCommentMaxLimit
	}
	var rows *sql.Rows
	var err error
	if cursor == "" {
		rows, err = d.QueryContext(ctx, `SELECT id, finding_id, body, author_kind, author_subject, created_at
FROM finding_comments WHERE finding_id=$1 ORDER BY id LIMIT $2`, findingID, limit+1)
	} else {
		after, cerr := decodeFindingCommentCursor(cursor)
		if cerr != nil {
			return nil, "", cerr
		}
		rows, err = d.QueryContext(ctx, `SELECT id, finding_id, body, author_kind, author_subject, created_at
FROM finding_comments WHERE finding_id=$1 AND id > $2 ORDER BY id LIMIT $3`, findingID, after, limit+1)
	}
	if err != nil {
		return nil, "", fmt.Errorf("list finding comments: %w", err)
	}
	defer rows.Close()
	out := []*FindingComment{}
	for rows.Next() {
		c := &FindingComment{}
		if err := rows.Scan(&c.ID, &c.FindingID, &c.Body, &c.AuthorKind, &c.AuthorSubject, &c.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("scan finding comment: %w", err)
		}
		c.CreatedAt = c.CreatedAt.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list finding comments: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		blob, _ := json.Marshal(findingCommentCursor{ID: out[limit-1].ID})
		next = base64.RawURLEncoding.EncodeToString(blob)
	}
	return out, next, nil
}
