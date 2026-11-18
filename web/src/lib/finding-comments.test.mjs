import {
  commentErrorMessage,
  countCodePoints,
  DEFAULT_MAX_COMMENT_RUNES,
  mergeComments,
  validateComment,
} from "./finding-comments.ts";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const comment = (id, body = `c${id}`) => ({
  id,
  finding_id: 7,
  body,
  author: { kind: "operator", subject: "ARTEX" },
  created_at: "2026-11-18T10:15:30.123456Z",
});

test("countCodePoints counts code points, not UTF-16 units", () => {
  assert.equal(countCodePoints(""), 0);
  assert.equal(countCodePoints("abc"), 3);
  assert.equal(countCodePoints("漢字"), 2);
  assert.equal(countCodePoints("😀"), 1);
  // family emoji: 3 emoji + 2 ZWJ = 5 code points (length would be 8)
  assert.equal(countCodePoints("👨\u200d👩\u200d👧"), 5);
});

test("validateComment normalises CRLF, trims, keeps newline and tab", () => {
  const r = validateComment("  a\r\nb\tc \r\n");
  assert.deepEqual(r, { ok: true, body: "a\nb\tc", length: 5 });
});

test("validateComment rejects empty and whitespace-only", () => {
  assert.equal(validateComment("").ok, false);
  assert.equal(validateComment(" \r\n\t ").ok, false);
  assert.equal(validateComment("").reason, "empty");
});

test("validateComment enforces the limit in code points", () => {
  assert.equal(validateComment("x".repeat(10000)).ok, true);
  const over = validateComment("x".repeat(10001));
  assert.equal(over.ok, false);
  assert.equal(over.reason, "too_long");
  // 10000 astral emoji are 20000 UTF-16 units but exactly at the limit
  assert.equal(validateComment("😀".repeat(10000)).ok, true);
  assert.equal(validateComment("😀".repeat(10001)).ok, false);
  assert.equal(validateComment("x".repeat(6), 5).ok, false);
  assert.equal(DEFAULT_MAX_COMMENT_RUNES, 10000);
});

test("validateComment accepts a ZWJ emoji sequence and ZWNJ, rejects U+200B", () => {
  assert.equal(validateComment("ok 👨\u200d👩\u200d👧").ok, true);
  assert.equal(validateComment("می\u200cخواهم").ok, true);
  const r = validateComment("a\u200bb");
  assert.equal(r.ok, false);
  assert.equal(r.reason, "forbidden_char");
});

test("validateComment rejects control, DEL, bidi, zero-width and lone surrogates", () => {
  for (const ch of [
    "\u0000",
    "\u0007",
    "\u001b",
    "\u007f",
    "\u202a",
    "\u202e",
    "\u2066",
    "\u2069",
    "\u2060",
    "\ufeff",
    "\ud800",
    "\u061c",
    "\u200e",
    "\u200f",
    "\u0080",
    "\u0085",
    "\u009f",
    "\u2061",
    "\u2064",
    "\ufff9",
    "\ufffb",
    "\u{e0000}",
    "\u{e0020}",
    "\u{e007f}",
  ]) {
    const r = validateComment(`a${ch}b`);
    assert.equal(r.ok, false, `U+${ch.codePointAt(0).toString(16)} must be rejected`);
    assert.equal(r.reason, "forbidden_char");
  }
  // a BOM at the edge is not trimmed (the server does not trim it either)
  assert.equal(validateComment("\ufeffabc").ok, false);
});

test("mergeComments dedupes by id and keeps id order", () => {
  const merged = mergeComments([comment(1), comment(3)], [comment(2), comment(3, "newer"), comment(4)]);
  assert.deepEqual(
    merged.map((c) => c.id),
    [1, 2, 3, 4],
  );
  assert.equal(merged[2].body, "newer");
  assert.deepEqual(mergeComments([], []), []);
});

test("mergeComments appends a created comment once even if a page also returns it", () => {
  const afterCreate = mergeComments([comment(1)], [comment(2)]);
  const afterLoadMore = mergeComments(afterCreate, [comment(2), comment(3)]);
  assert.deepEqual(
    afterLoadMore.map((c) => c.id),
    [1, 2, 3],
  );
});

test("commentErrorMessage maps statuses to fixed text and passes server messages through", () => {
  const fallback = (status) => `POST /exploration/findings/7/comments: ${status}`;
  for (const status of [400, 403, 404, 409, 413, 415]) {
    const msg = commentErrorMessage(new Error(fallback(status)));
    assert.ok(msg.length > 0 && !msg.includes("/exploration"), `status ${status}`);
  }
  assert.equal(
    commentErrorMessage(new Error("comment limit reached for this finding")),
    "comment limit reached for this finding",
  );
  assert.equal(commentErrorMessage(new Error("Unauthorized")), "Unauthorized");
  assert.equal(commentErrorMessage("boom"), "boom");
});

test("panel source renders text only: no raw HTML, markdown or link detection", () => {
  const src = readFileSync(new URL("../components/finding-comments-panel.tsx", import.meta.url), "utf8");
  assert.ok(!src.includes("dangerouslySetInnerHTML"));
  assert.ok(!/markdown/i.test(src));
  assert.ok(!/react-markdown|linkify|<a[\s>]/i.test(src));
  assert.ok(src.includes("whitespace-pre-wrap"));
  assert.ok(!/toast\.[a-z]+\([^)]*(body|draft)/i.test(src));
});

test("validateComment accepts variation selectors, soft hyphen and line separators", () => {
  assert.equal(validateComment("love \u2764\ufe0f").ok, true);
  assert.equal(validateComment("\u2764\ufe0e").ok, true);
  assert.equal(validateComment("co\u00adoperate").ok, true);
  assert.equal(validateComment("a\u2028b\u2029c").ok, true);
});

test("validateComment trims NEL at the edges but rejects it inside", () => {
  assert.deepEqual(validateComment("\u0085 x \u0085"), { ok: true, body: "x", length: 1 });
  assert.equal(validateComment("a\u0085b").ok, false);
});
