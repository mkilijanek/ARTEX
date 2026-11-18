// Pure helpers for the finding comments panel. Kept free of `@/` alias imports so
// `node --test` can load this file directly. The server (db.NormalizeFindingCommentBody)
// stays authoritative; these checks only give early feedback.
import type { FindingComment } from "./types";

// Fallback until the first list response reports the server's `max_body_runes`.
export const DEFAULT_MAX_COMMENT_RUNES = 10000;

// Code points, not UTF-16 units: matches the server's rune count.
export function countCodePoints(s: string): number {
  let n = 0;
  for (const _ of s) n++;
  return n;
}

// Same set as Go's unicode.IsSpace, so edge trimming agrees with the server (JS trim()
// would also strip U+FEFF, which the server rejects instead).
const EDGE_SPACE =
  /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g;

// Mirrors db.NormalizeFindingCommentBody in db/finding_comments.go: change both together.
// Rejected: C0 controls except \n and \t, DEL, C1 controls U+0080-U+009F (incl. NEL, which
// is only trimmed at the edges), bidi marks/controls U+061C U+200E U+200F U+202A-202E
// U+2066-2069, invisible U+200B U+2060-U+2064 U+FEFF U+FFF9-U+FFFB, tags U+E0000-U+E007F
// and lone surrogates (invalid UTF-8 on the wire). Allowed: ZWNJ U+200C, ZWJ U+200D,
// variation selectors, soft hyphen U+00AD, U+2028/U+2029.
const FORBIDDEN =
  // biome-ignore lint/suspicious/noControlCharactersInRegex: these characters are rejected on purpose (mirrors the server rule)
  /[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u061c\u200b\u200e\u200f\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff\ufff9-\ufffb\u{e0000}-\u{e007f}]|\p{Surrogate}/u;

export type CommentRejection = "empty" | "too_long" | "forbidden_char";

export const REJECTION_MESSAGES: Record<CommentRejection, string> = {
  empty: "Write a comment first.",
  too_long: "The comment is too long.",
  forbidden_char:
    "The comment contains a character that is not allowed (control, bidirectional or zero-width characters). Put such payloads in evidence instead.",
};

export type CommentValidation =
  | { ok: true; body: string; length: number }
  | { ok: false; reason: CommentRejection; message: string; length: number };

export function validateComment(raw: string, max: number = DEFAULT_MAX_COMMENT_RUNES): CommentValidation {
  const body = raw.replace(/\r\n/g, "\n").replace(EDGE_SPACE, "");
  const length = countCodePoints(body);
  const reject = (reason: CommentRejection): CommentValidation => ({
    ok: false,
    reason,
    message: REJECTION_MESSAGES[reason],
    length,
  });
  if (body === "") return reject("empty");
  if (length > max) return reject("too_long");
  if (FORBIDDEN.test(body)) return reject("forbidden_char");
  return { ok: true, body, length };
}

// Merge pages and freshly created comments: unique by id (later copy wins), id order.
export function mergeComments(current: FindingComment[], incoming: FindingComment[]): FindingComment[] {
  const byId = new Map<number, FindingComment>();
  for (const c of current) byId.set(c.id, c);
  for (const c of incoming) byId.set(c.id, c);
  return [...byId.values()].sort((a, b) => a.id - b.id);
}

const STATUS_MESSAGES: Record<number, string> = {
  400: "The server rejected the comment.",
  403: "Comments cannot be added to a finding viewed through an inherited task.",
  404: "This finding no longer exists.",
  409: "Comments cannot be added right now (the finding's task is being archived, or the comment limit is reached).",
  413: "The comment is too large.",
  415: "The request was not sent as JSON.",
};

// api.ts `http()` throws the server's `{"error"}` text when present, else
// "<METHOD> <path>: <status>". Show the server's fixed message as is; map the bare
// status fallback to readable text.
export function commentErrorMessage(e: unknown): string {
  const message = e instanceof Error ? e.message : String(e);
  const m = /: (\d{3})$/.exec(message);
  if (m) {
    const mapped = STATUS_MESSAGES[Number(m[1])];
    if (mapped) return mapped;
  }
  return message;
}
