// Pure helpers for the Signal account card: the status-to-view mapping, the failure classes of the
// Signal routes, and the request transport. Kept free of `@/` alias imports so `node --test` can load
// this file directly. The server stays authoritative; the card only derives what to show from it.
import type { SignalAccountStatus } from "./types";

// A heartbeat older than this, or a missing one, counts as a stale sidecar.
export const SIGNAL_HEARTBEAT_STALE_S = 30;

// Client-side phase of a link request the server has not reported as linking yet.
export type SignalLinkPhase = "idle" | "preparing" | "fetch_failed";

export type SignalWarning =
  | { kind: "db_role_missing" }
  | { kind: "db_role_error"; detail: string }
  | { kind: "db_role_unchecked" }
  | { kind: "db_auth_trust" }
  | { kind: "db_auth_unchecked" }
  | { kind: "db_auth_asserted" }
  | { kind: "other"; text: string };

export type SignalCardState =
  | { kind: "off" }
  | { kind: "no_service" }
  | { kind: "unknown" }
  | { kind: "preparing" }
  | { kind: "preparing_failed" }
  | { kind: "linking"; secondsLeft: number }
  | { kind: "linking_stale"; secondsLeft: number }
  | { kind: "expired" }
  | { kind: "unlinked"; removedOnSignal: "removed" | "local_only" | null; lastError: string | null }
  | { kind: "linked_unconfirmed"; numberHint: string }
  | { kind: "linked_confirmed"; numberHint: string; groupsCount: number; groupsAt: string | null }
  | { kind: "error"; lastError: string | null; lastErrorAt: string | null };

export interface SignalCardView {
  state: SignalCardState;
  warnings: SignalWarning[];
}

function remainingMs(expiresAt: string, nowMs: number): number {
  const ms = Date.parse(expiresAt);
  if (Number.isNaN(ms)) throw new Error("signal status: invalid link expiry");
  return ms - nowMs;
}

function wholeSecondsUp(ms: number): number {
  return Math.ceil(ms / 1000);
}

export function signalWarnings(status: SignalAccountStatus): SignalWarning[] {
  const out: SignalWarning[] = [];
  if (status.db_role.state === "missing") out.push({ kind: "db_role_missing" });
  if (status.db_role.state === "error") out.push({ kind: "db_role_error", detail: status.db_role.detail });
  if (status.db_role.state === "unchecked") out.push({ kind: "db_role_unchecked" });
  if (status.db_auth === "trust") out.push({ kind: "db_auth_trust" });
  if (status.db_auth === "unchecked") out.push({ kind: "db_auth_unchecked" });
  if (status.db_auth === "asserted") out.push({ kind: "db_auth_asserted" });
  for (const text of status.warnings) out.push({ kind: "other", text });
  return out;
}

function cardState(status: SignalAccountStatus, nowMs: number, phase: SignalLinkPhase): SignalCardState {
  const fresh = status.sidecar.alive && status.sidecar.heartbeat_age_s <= SIGNAL_HEARTBEAT_STALE_S;

  if (status.state === "linking") {
    if (!status.linking) throw new Error("signal status: linking without a link expiry");
    const ms = remainingMs(status.linking.expires_at, nowMs);
    if (ms <= 0) return { kind: "expired" };
    if (!fresh) return { kind: "linking_stale", secondsLeft: wholeSecondsUp(ms) };
    if (phase === "fetch_failed") return { kind: "preparing_failed" };
    if (phase === "preparing") return { kind: "preparing" };
    return { kind: "linking", secondsLeft: wholeSecondsUp(ms) };
  }

  if (!fresh) return { kind: "no_service" };

  switch (status.state) {
    case "unknown":
      return { kind: "unknown" };
    case "unlinked":
      if (phase === "preparing") return { kind: "preparing" };
      if (phase === "fetch_failed") return { kind: "preparing_failed" };
      return {
        kind: "unlinked",
        removedOnSignal: status.remote_unlink || null,
        lastError: status.last_error || null,
      };
    case "linked":
      if (status.account.confirmed) {
        return {
          kind: "linked_confirmed",
          numberHint: status.account.number_hint,
          groupsCount: status.groups_count,
          groupsAt: status.groups_at,
        };
      }
      return { kind: "linked_unconfirmed", numberHint: status.account.number_hint };
    case "error":
      return {
        kind: "error",
        lastError: status.last_error || null,
        lastErrorAt: status.last_error_at,
      };
    default:
      throw new Error("signal status: unexpected state");
  }
}

// signalCardView maps the status payload to the one card state to show. nowMs is passed in so the
// countdowns stay a pure function of their inputs. phase is the browser's own link request progress.
export function signalCardView(
  status: SignalAccountStatus,
  nowMs: number,
  phase: SignalLinkPhase = "idle",
): SignalCardView {
  if (status.mode === "off") return { state: { kind: "off" }, warnings: [] };
  return { state: cardState(status, nowMs, phase), warnings: signalWarnings(status) };
}

export class SignalRequestError extends Error {
  status: number;
  code: string | null;
  retryAfterS: number | null;

  constructor(status: number, code: string | null, retryAfterS: number | null) {
    super(`Signal request failed: ${status}${code ? ` (${code})` : ""}`);
    this.name = "SignalRequestError";
    this.status = status;
    this.code = code;
    this.retryAfterS = retryAfterS;
  }
}

// parseSignalError reads the machine-readable code from a JSON error body and the retry delay from the
// Retry-After header (seconds only).
export function parseSignalError(status: number, body: string, retryAfter: string | null): SignalRequestError {
  let code: string | null = null;
  if (body) {
    try {
      const parsed: unknown = JSON.parse(body);
      if (parsed && typeof parsed === "object" && "code" in parsed && typeof parsed.code === "string" && parsed.code) {
        code = parsed.code;
      }
    } catch {
      // A non-JSON error body has no code. The status still reaches the caller.
    }
  }
  const seconds = retryAfter ? Number(retryAfter) : Number.NaN;
  const retryAfterS = Number.isFinite(seconds) && seconds >= 0 ? Math.ceil(seconds) : null;
  return new SignalRequestError(status, code, retryAfterS);
}

export type SignalFailureKind =
  | "password_incorrect"
  | "number_mismatch"
  | "link_pending"
  | "already_linked"
  | "unlink_first"
  | "not_ready"
  | "conflict"
  | "throttled"
  | "unavailable"
  | "failed";

// signalFailureKind names what a failed Signal request means for the UI.
export function signalFailureKind(err: { status: number; code: string | null }): SignalFailureKind {
  if (err.status === 403) {
    if (err.code === "password_incorrect") return "password_incorrect";
    if (err.code === "number_mismatch") return "number_mismatch";
    return "failed";
  }
  if (err.status === 409) {
    if (err.code === "link_pending") return "link_pending";
    if (err.code === "already_linked") return "already_linked";
    if (err.code === "unlink_first") return "unlink_first";
    if (err.code === "not_ready") return "not_ready";
    return "conflict";
  }
  if (err.status === 429) return "throttled";
  if (err.status === 503) return "unavailable";
  return "failed";
}

export interface SignalTransport {
  fetch: (url: string, init: RequestInit) => Promise<Response>;
  token: string | null;
  // Called on 401 only. A wrong password (403) is an answer to the request, not a lost session.
  onUnauthorized: () => void;
}

// signalRequest sends one request to the Signal routes. The token goes in the Authorization header and a
// JSON body gets a JSON content type. An empty success body resolves to undefined.
export async function signalRequest<T>(t: SignalTransport, method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (t.token) headers.Authorization = `Bearer ${t.token}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const r = await t.fetch(`/api${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (r.status === 401) {
    t.onUnauthorized();
    throw new Error("Unauthorized");
  }
  const text = await r.text();
  if (!r.ok) throw parseSignalError(r.status, text, r.headers.get("Retry-After"));
  return (text ? JSON.parse(text) : undefined) as T;
}
