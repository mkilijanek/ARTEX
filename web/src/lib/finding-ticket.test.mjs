import { MAX_TICKET_ID_LENGTH, normalizeTicketId, ticketIdErrorMessage } from "./finding-ticket.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("normalizeTicketId trims surrounding whitespace", () => {
  assert.deepEqual(normalizeTicketId("  SEC-1234 \t"), { ok: true, value: "SEC-1234" });
});

test("normalizeTicketId accepts empty and whitespace-only as a clear", () => {
  assert.deepEqual(normalizeTicketId(""), { ok: true, value: "" });
  assert.deepEqual(normalizeTicketId("  \n "), { ok: true, value: "" });
});

test("normalizeTicketId keeps inner spaces", () => {
  assert.deepEqual(normalizeTicketId("INC 0012"), { ok: true, value: "INC 0012" });
});

test("normalizeTicketId rejects inner line breaks and control characters", () => {
  for (const bad of ["A\nB", "A\rB", "A\tB", "A\u0000B", "A\u007fB", "A\u0085B", "A\u2028B", "A\u2029B"]) {
    assert.deepEqual(normalizeTicketId(bad), { ok: false, reason: "control" }, JSON.stringify(bad));
  }
});

test("normalizeTicketId enforces the length limit in code points", () => {
  assert.equal(normalizeTicketId("a".repeat(MAX_TICKET_ID_LENGTH)).ok, true);
  assert.deepEqual(normalizeTicketId("a".repeat(MAX_TICKET_ID_LENGTH + 1)), { ok: false, reason: "too_long" });
  assert.equal(normalizeTicketId("😀".repeat(MAX_TICKET_ID_LENGTH)).ok, true);
  assert.equal(normalizeTicketId("😀".repeat(MAX_TICKET_ID_LENGTH + 1)).ok, false);
});

test("normalizeTicketId measures length after trimming", () => {
  assert.equal(normalizeTicketId(` ${"a".repeat(MAX_TICKET_ID_LENGTH)} `).ok, true);
});

test("ticketIdErrorMessage names the violated rule", () => {
  assert.match(ticketIdErrorMessage("too_long"), /128/);
  assert.match(ticketIdErrorMessage("control"), /line breaks/);
});
