# Finding Comments

Analysts can attach plain-text comments to a vulnerability (finding): triage notes, false-positive reasoning, hand-off context. Comments are listed under the finding details, oldest first, and are append-only.

Comments are for people only. They never reach agent prompts, chat mentions, retest snapshots or exports: they live in their own table and are not part of the finding object that those paths read.

## Data model

Table `finding_comments` (`db/schema.sql`), created automatically on startup:

| Column | Notes |
|---|---|
| `id` | `BIGSERIAL` primary key; defines order and paging |
| `finding_id` | references `findings(id)` `ON DELETE CASCADE` |
| `body` | text, non-blank, at most 10000 characters (database CHECK) |
| `author_kind` | always `operator` today |
| `author_subject` | verified token subject, 1..200 characters |
| `created_at` | `clock_timestamp()` |

Index `idx_finding_comments_page` on `(finding_id, id)`.

Comments are deleted with their finding. Retests, report updates and status changes do not touch them.

## API

Both routes use the same access rules as the other finding detail endpoints (`findingTrafficAccess`), including the optional `context_task` query parameter.

### `GET /api/exploration/findings/{id}/comments`

Query: `limit` (default 50, max 200), `cursor` (opaque value from `next_cursor`), `context_task`.

```json
{
  "comments": [
    {"id": 7, "finding_id": 42, "body": "Confirmed on staging.",
     "author": {"kind": "operator", "subject": "ARTEX"},
     "created_at": "2026-11-18T10:00:00.123Z"}
  ],
  "next_cursor": "",
  "max_body_runes": 10000
}
```

Comments are ordered by `id` ascending. An empty `next_cursor` means there are no further pages.

### `POST /api/exploration/findings/{id}/comments`

Request: `Content-Type: application/json` (parameters such as `charset` are allowed; no other media type), body at most 128 KiB, exactly one JSON object with one field:

```json
{"body": "Confirmed on staging."}
```

Unknown fields and trailing data are rejected. The author is never taken from the request: it is `operator` plus the subject of the verified token (today the single shared account `ARTEX`). Response `201` with `{"comment": {...}}` in the shape above.

### Status codes

Error bodies use the server's usual error format with these fixed messages. Messages never quote the submitted text.

| Status | Message | When |
|---|---|---|
| 400 | `invalid finding id` | `{id}` is not a valid id |
| 400 | `invalid cursor` | GET with a malformed cursor |
| 400 | `invalid JSON` / `unknown field` / `body is not valid UTF-8` | malformed request |
| 400 | `body must not be empty`, `body is too long`, `body contains control characters`, `body contains bidirectional control characters`, `body contains zero-width characters` | body rules below |
| 400 | `comment rejected` | database CHECK rejected a body that passed validation (should not happen) |
| 401 | `no operator identity` | POST without a verified token subject |
| 403 | `inherited findings are read-only; modify them in the source task` | POST in an inherited `context_task` view |
| 404 | `finding not found`, `context task not found`, `finding not available in task context` | |
| 409 | `the finding's task is being archived; comments are read-only` | task archive in progress |
| 409 | `comment limit reached for this finding` | 2000 comments already exist |
| 413 | `request body too large` | request over 128 KiB |
| 415 | `Content-Type must be application/json` | |
| 500 | `failed to load comments` / `failed to save comment` | database error (details only in the server log) |
| 500 | database error text | the shared finding-access check failed to load the finding; this existing helper returns the error text as is (it never contains comment text) |
| 503 | `management console data source (PostgreSQL) is not connected` | no database |

## Body rules

Applied server-side by `db.NormalizeFindingCommentBody` and mirrored in `web/src/lib/finding-comments.ts` (change both together):

- CRLF becomes LF; leading and trailing whitespace is trimmed.
- Length 1..10000 Unicode code points after trimming.
- Rejected: C0 controls except newline and tab; U+007F-U+009F; bidi controls U+061C, U+200E, U+200F, U+202A-U+202E, U+2066-U+2069; invisible characters U+200B, U+2060-U+2064, U+FEFF, U+FFF9-U+FFFB and tags U+E0000-U+E007F.
- Allowed on purpose: ZWNJ (U+200C), ZWJ (U+200D), variation selectors, soft hyphen, U+2028/U+2029.

## UI

The "Comments" panel in the finding details lists comments with author and time, loads further pages on demand, and has a text box limited to `max_body_runes` code points. Bodies are rendered as plain text (`whitespace-pre-wrap`): no Markdown, HTML or link detection. In an inherited task view the panel is read-only and points to the source task.

## Archives

Task archives include the comments of the task's findings (`finding_comments` table in the package); the package format stays 3. Restore validates every row (the finding belongs to the archived task, body rules, author, per-finding cap) and fails as a whole on any bad row. Packages without comments restore none.

## Rollback

A binary older than this feature ignores the table. Before downgrading:

1. `pg_dump --data-only --table=finding_comments` and keep the file.
2. Deploy the previous binary. The table stays.
3. While on the old binary, do not archive tasks whose findings have comments (the cascade removes the comments without packaging them), and do not restore packages whose manifest has `data_counts.finding_comments > 0` (they restore without comments and the package is then removed).
4. After re-upgrading, re-import comments of still existing findings from the dump.

Dropping the table (`DROP TABLE finding_comments;`) removes the data permanently.

## Observability

Server log lines (also under `/api/logs`), never with comment content:

```
[finding-comments] created finding=42 comment=7 author=operator/ARTEX runes=21
[finding-comments] create failed finding=42 status=500 sqlstate=... constraint="..."
[finding-comments] list failed finding=42 status=500 ...
```

## Decisions

- **Separate table, ordered by id.** Ids are assigned under the finding row lock and give a stable, gap-tolerant cursor; timestamps can tie.
- **Plain text.** Notes often quote target-controlled content. Markdown would render remote images and links from it and reformat pasted payloads. The raw text is stored, so Markdown can be added later without a data change.
- **Shared-account attribution.** ARTEX has one built-in operator account. The author comes from the verified token only, so it cannot be forged; `author_kind` has no CHECK so named users can be added later.
- **Archives carry comments.** Otherwise archiving and restoring a task would silently destroy analyst work.
- **Inherited views are read-only**, reusing the existing finding access helper.
- **Control and invisible characters are rejected** so a note cannot display differently from what is stored; ZWNJ/ZWJ stay allowed for emoji and scripts that need them.
- **Cap of 2000 comments per finding** bounds growth, as there is no request rate limit.

## Out of scope

Editing or deleting comments, mentions, notifications, Markdown, per-person identity, and feeding comments to agents.

## Known limits

- Attribution cannot tell analysts apart: every comment is `operator/ARTEX`.
- No per-user access rules exist on finding endpoints; any token holder can read and add comments.
- Comments are stored as clear text. They are included in task archive packages and in database backups; treat those files accordingly.
- Residual, not verified: an agent with shell or MCP egress that also holds an operator token could read comments through the API. The destination policy for WebFetch and the custom http tool blocks ARTEX's own ports; shell and MCP egress are not covered.
- UI states are verified by a scripted browser run, not by automated tests. `npm test` in `web/` covers the pure helpers only.
