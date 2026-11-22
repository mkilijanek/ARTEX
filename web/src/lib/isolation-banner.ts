import type { ToolIsolation } from "./types";

export type IsolationBannerKind = "none" | "unavailable" | "degraded" | "notice" | "unknown";

// Documentation section named in the hardening plan ("Tool isolation").
export const ISOLATION_DOCS_URL = "https://github.com/mkilijanek/ARTEX/blob/main/docs/tool-isolation.md";

// Maps the server status to a banner: unavailable/disabled cannot be dismissed, degraded can be collapsed,
// an enforced status that carries warnings shows a dismissible notice. A missing status shows nothing (not
// fetched yet, or a server that predates the status); a status string this page does not know is the
// "unknown" notice, never "none".
export function isolationBannerKind(t: ToolIsolation | null | undefined): IsolationBannerKind {
  if (!t) return "none";
  switch (t.status) {
    case "unavailable":
    case "disabled":
      return "unavailable";
    case "degraded":
      return "degraded";
    case "enforced":
      return (t.warnings ?? []).length > 0 ? "notice" : "none";
    case "inactive":
      return "none";
    default:
      return "unknown";
  }
}

// The status is refreshed this often (and when the window gets focus).
export const ISOLATION_POLL_MS = 60_000;
// This many failed refreshes in a row replace the last known status with the unknown notice.
export const ISOLATION_UNKNOWN_AFTER = 2;

export interface IsolationState {
  isolation: ToolIsolation | null;
  failures: number;
}

export type IsolationFetch = { ok: true; isolation: ToolIsolation | null | undefined } | { ok: false };

// A failed refresh keeps the last known status and counts; a success replaces it and resets the count.
export function nextIsolationState(prev: IsolationState, r: IsolationFetch): IsolationState {
  if (r.ok) return { isolation: r.isolation ?? null, failures: 0 };
  return { isolation: prev.isolation, failures: prev.failures + 1 };
}

const UNKNOWN_ISOLATION: ToolIsolation = { status: "unknown", reason: "", warnings: [] };

// What the banner shows: the last known status, or the unknown notice after enough failures in a row.
export function displayedIsolation(s: IsolationState): ToolIsolation | null {
  return s.failures >= ISOLATION_UNKNOWN_AFTER ? UNKNOWN_ISOLATION : s.isolation;
}

export function isolationBannerText(t: ToolIsolation): { title: string; body: string } {
  const kind = isolationBannerKind(t);
  if (kind === "unavailable") {
    return {
      title: "Tool isolation is not active",
      body:
        t.status === "disabled"
          ? "Tool isolation was turned off by the operator, so agent tools run with this server's own access. Remove the override or run the official Docker image with its default settings."
          : "Agent tools run with this server's own access. Run the official Docker image with its default settings, or follow the tool isolation documentation.",
    };
  }
  if (kind === "unknown") {
    return {
      title: "Tool isolation status is unknown",
      body: "The server's tool isolation status could not be read or is not recognised. Agent tools may run without isolation. Check the server log and the tool isolation documentation.",
    };
  }
  if (kind === "degraded") {
    return {
      title: "Tool isolation is only partly active",
      body: "Some protections could not be confirmed. Check the server log and follow the tool isolation documentation.",
    };
  }
  return {
    title: "Tool isolation is active, with warnings",
    body: "Check the server log for details; the tool isolation documentation explains each warning.",
  };
}
