// Pure helper for the finding Ticket ID field (issue #72). Kept free of `@/` alias imports so
// `node --test` can load this file directly. Mirrors the server rules: trim surrounding whitespace,
// reject line breaks and control characters, at most 128 characters (counted in code points).
export const MAX_TICKET_ID_LENGTH = 128;

export type TicketIdResult = { ok: true; value: string } | { ok: false; reason: "control" | "too_long" };

export function normalizeTicketId(raw: string): TicketIdResult {
  const value = raw.trim();
  // \p{Cc} covers C0/C1 controls incl. CR, LF, TAB; U+2028/2029 are the Unicode line/paragraph separators.
  if (/[\p{Cc}\u2028\u2029]/u.test(value)) return { ok: false, reason: "control" };
  if ([...value].length > MAX_TICKET_ID_LENGTH) return { ok: false, reason: "too_long" };
  return { ok: true, value };
}

export function ticketIdErrorMessage(reason: "control" | "too_long"): string {
  return reason === "too_long"
    ? `Ticket ID must be at most ${MAX_TICKET_ID_LENGTH} characters`
    : "Ticket ID must not contain line breaks or control characters";
}
