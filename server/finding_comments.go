package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db"
)

// Analyst comments on findings (issue #65). Comments are a sub-resource of a
// finding and are deliberately not part of FindingDTO: nothing here reaches
// chat mentions, retest snapshots or exports.

const (
	maxFindingCommentRequestBytes = 128 << 10
	findingCommentAuthorOperator  = "operator"
	findingCommentLogTag          = "[finding-comments]"
)

func (s *Server) registerFindingComments(mux *http.ServeMux) {
	base := "/api/exploration/findings/{id}/comments"
	mux.HandleFunc("GET "+base, s.listFindingComments)
	mux.HandleFunc("POST "+base, s.createFindingComment)
}

type findingCommentAuthorJSON struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

type findingCommentJSON struct {
	ID        int64                    `json:"id"`
	FindingID int64                    `json:"finding_id"`
	Body      string                   `json:"body"`
	Author    findingCommentAuthorJSON `json:"author"`
	CreatedAt string                   `json:"created_at"`
}

func findingCommentToJSON(c *db.FindingComment) findingCommentJSON {
	return findingCommentJSON{
		ID: c.ID, FindingID: c.FindingID, Body: c.Body,
		Author:    findingCommentAuthorJSON{Kind: c.AuthorKind, Subject: c.AuthorSubject},
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// logFindingCommentFailure writes one content-free line. A database error is
// reduced to SQLSTATE and constraint name: the driver's Detail quotes the row.
func logFindingCommentFailure(op string, findingID int64, status int, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		log.Printf("%s %s failed finding=%d status=%d sqlstate=%s constraint=%q",
			findingCommentLogTag, op, findingID, status, pgErr.Code, pgErr.ConstraintName)
		return
	}
	log.Printf("%s %s failed finding=%d status=%d: %v", findingCommentLogTag, op, findingID, status, err)
}

func (s *Server) listFindingComments(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := findingPaginationParam(q.Get("limit"), 50, 200)
	items, next, err := pg.ListFindingComments(r.Context(), id, q.Get("cursor"), limit)
	if errors.Is(err, db.ErrFindingCommentCursor) {
		writeErr(w, 400, "invalid cursor")
		return
	}
	if err != nil {
		logFindingCommentFailure("list", id, 500, err)
		writeErr(w, 500, "failed to load comments")
		return
	}
	out := make([]findingCommentJSON, 0, len(items))
	for _, c := range items {
		out = append(out, findingCommentToJSON(c))
	}
	writeJSON(w, 200, map[string]any{
		"comments":       out,
		"next_cursor":    next,
		"max_body_runes": db.MaxFindingCommentRunes,
	})
}

// jsonMediaType reports whether the Content-Type is exactly application/json
// (parameters such as charset are allowed). No prefix or substring matching.
func jsonMediaType(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

func (s *Server) createFindingComment(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	subject := operatorSubject(r)
	if subject == "" {
		writeErr(w, 401, "no operator identity")
		return
	}
	if !jsonMediaType(r) {
		writeErr(w, 415, "Content-Type must be application/json")
		return
	}
	id, ok := s.findingTrafficAccess(w, r, true)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFindingCommentRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, 413, "request body too large")
			return
		}
		logFindingCommentFailure("read request", id, 400, err)
		writeErr(w, 400, "invalid JSON")
		return
	}
	// encoding/json silently replaces invalid UTF-8 with U+FFFD; reject it instead.
	if !utf8.Valid(raw) {
		writeErr(w, 400, "body is not valid UTF-8")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err = dec.Decode(&req)
	if err == nil {
		// Exactly one JSON value: anything after it is an error.
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field") {
			writeErr(w, 400, "unknown field")
		} else {
			writeErr(w, 400, "invalid JSON")
		}
		return
	}
	body, err := db.NormalizeFindingCommentBody(req.Body)
	if err != nil {
		// The store's messages are fixed per rule and never quote the input.
		writeErr(w, 400, strings.TrimPrefix(err.Error(), db.ErrFindingCommentInvalid.Error()+": "))
		return
	}
	c, err := pg.AddFindingComment(r.Context(), id, findingCommentAuthorOperator, subject, body)
	switch {
	case err == nil:
	case errors.Is(err, db.ErrFindingNotFound):
		writeErr(w, 404, "finding not found")
		return
	case errors.Is(err, db.ErrTaskArchiveState):
		writeErr(w, 409, "the finding's task is being archived; comments are read-only")
		return
	case errors.Is(err, db.ErrFindingCommentLimit):
		writeErr(w, 409, db.ErrFindingCommentLimit.Error())
		return
	case errors.Is(err, db.ErrFindingCommentInvalid):
		// Validation passed above, so this is a database CHECK (validation drift).
		log.Printf("%s create failed finding=%d status=400 sqlstate=23514: %v", findingCommentLogTag, id, err)
		writeErr(w, 400, "comment rejected")
		return
	default:
		logFindingCommentFailure("create", id, 500, err)
		writeErr(w, 500, "failed to save comment")
		return
	}
	log.Printf("%s created finding=%d comment=%d author=%s/%s runes=%d",
		findingCommentLogTag, c.FindingID, c.ID, c.AuthorKind, c.AuthorSubject, utf8.RuneCountInString(c.Body))
	writeJSON(w, 201, map[string]any{"comment": findingCommentToJSON(c)})
}
