"use client";

import * as React from "react";

import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import { MOCK } from "@/lib/mock/enabled";
import {
  type SignalCardView,
  type SignalLinkPhase,
  SignalRequestError,
  type SignalWarning,
  signalCardView,
  signalServiceLine,
} from "@/lib/signal-account";
import type { SignalAccountStatus, SignalGroup, SignalLinkURI } from "@/lib/types";

import { relativeAge } from "./channel-fields";
import {
  SignalConfirmForm,
  SignalLinkPanel,
  SignalLinkStartDialog,
  SignalUnlinkDialog,
  signalErrorText,
  signalFailureIsStale,
} from "./signal-link-dialog";

const FAST_POLL_MS = 3000;
const SLOW_POLL_MS = 15000;
// How long the card waits for the service to publish the link code before it offers a retry.
const PREPARE_TIMEOUT_MS = 20000;
// How long a group refresh may take before the card stops waiting.
const GROUPS_REFRESH_TIMEOUT_MS = 30000;

export const SIGNAL_OFF_TEXT =
  "Signal is not set up in this deployment. Add the Signal override (see the deployment README).";

export interface SignalAccountApi {
  // mock is true when the app runs on mock data: there is no Signal service to ask.
  mock: boolean;
  status: SignalAccountStatus | null;
  view: SignalCardView | null;
  loadError: string | null;
  viewError: string | null;
  uri: SignalLinkURI | null;
  groups: SignalGroup[] | null;
  groupsError: string | null;
  refreshingGroups: boolean;
  justUnlinked: boolean;
  nowMs: number;
  reload: () => void;
  startLink: (v: { password: string; device_name: string; acknowledge: boolean }) => Promise<void>;
  retryLink: () => void;
  cancelLink: () => Promise<void>;
  confirm: (digits: string) => Promise<void>;
  unlink: (password: string) => Promise<void>;
  refreshGroups: () => Promise<void>;
}

// useSignalAccount owns the status polling and the link flow for the card and the channel form. The link
// code is kept in component state only: it is not stored, logged or sent anywhere.
export function useSignalAccount(): SignalAccountApi {
  const [status, setStatus] = React.useState<SignalAccountStatus | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [phase, setPhase] = React.useState<SignalLinkPhase>("idle");
  const [uri, setUri] = React.useState<SignalLinkURI | null>(null);
  const [groups, setGroups] = React.useState<SignalGroup[] | null>(null);
  const [groupsError, setGroupsError] = React.useState<string | null>(null);
  const [refreshFrom, setRefreshFrom] = React.useState<{ at: string | null } | null>(null);
  const [justUnlinked, setJustUnlinked] = React.useState(false);
  const [nowMs, setNowMs] = React.useState(() => Date.now());
  const [requestedAtMs, setRequestedAtMs] = React.useState<number | null>(null);
  const preparingSince = React.useRef(0);
  const fetchingURI = React.useRef(false);
  const loadSeq = React.useRef(0);
  const mounted = React.useRef(true);

  React.useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const load = React.useCallback(() => {
    if (MOCK) return;
    const seq = ++loadSeq.current;
    api
      .signalAccount()
      .then((s) => {
        // A slower, older answer must not overwrite a newer one.
        if (!mounted.current || seq !== loadSeq.current) return;
        setStatus(s);
        setLoadError(null);
      })
      .catch((e) => {
        if (!mounted.current || seq !== loadSeq.current) return;
        setLoadError(`Could not load the Signal status: ${(e as Error).message}`);
      });
  }, []);

  const linkingNow = status?.state === "linking";
  const fast = linkingNow || phase !== "idle" || refreshFrom !== null;

  React.useEffect(() => {
    if (MOCK) return;
    load();
    const id = setInterval(load, fast ? FAST_POLL_MS : SLOW_POLL_MS);
    return () => clearInterval(id);
  }, [load, fast]);

  // One-second clock while a countdown or a preparing timeout is running.
  const ticking = linkingNow || phase === "preparing";
  React.useEffect(() => {
    if (!ticking) return;
    setNowMs(Date.now());
    const id = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(id);
  }, [ticking]);

  // A status the client cannot read is shown as an error line, not as a half-drawn card.
  const { view, viewError } = React.useMemo<{ view: SignalCardView | null; viewError: string | null }>(() => {
    if (!status) return { view: null, viewError: null };
    try {
      return { view: signalCardView(status, nowMs, phase, requestedAtMs), viewError: null };
    } catch (e) {
      return { view: null, viewError: (e as Error).message };
    }
  }, [status, nowMs, phase, requestedAtMs]);

  const fetchURI = React.useCallback(() => {
    if (fetchingURI.current) return;
    fetchingURI.current = true;
    setPhase("preparing");
    preparingSince.current = Date.now();
    api
      .signalLinkURI()
      .then((u) => {
        if (!mounted.current) return;
        setUri(u);
        setPhase("idle");
      })
      .catch(() => {
        if (!mounted.current) return;
        setPhase("fetch_failed");
      })
      .finally(() => {
        fetchingURI.current = false;
      });
  }, []);

  // The server reports a link in progress and the browser has no code yet (right after the start, or
  // after a page reload): fetch it once.
  React.useEffect(() => {
    if (status?.state !== "linking") {
      if (uri) setUri(null);
      return;
    }
    if (!uri && (phase === "idle" || phase === "preparing")) fetchURI();
  }, [status?.state, uri, phase, fetchURI]);

  // After the start was accepted, the service may take a moment to publish the code. Give up waiting.
  React.useEffect(() => {
    if (phase !== "preparing" || status?.state === "linking") return;
    if (nowMs - preparingSince.current > PREPARE_TIMEOUT_MS) setPhase("fetch_failed");
  }, [phase, status?.state, nowMs]);

  const confirmed = status?.state === "linked" && status.account.confirmed;
  const groupsAt = status?.groups_at ?? null;
  // biome-ignore lint/correctness/useExhaustiveDependencies: a newer groups_at from the service intentionally reloads the list.
  React.useEffect(() => {
    if (!confirmed) {
      setGroups(null);
      setGroupsError(null);
      return;
    }
    let live = true;
    api
      .signalGroups()
      .then((g) => {
        if (!live) return;
        setGroups(g.groups);
        setGroupsError(null);
      })
      .catch((e) => {
        if (!live) return;
        setGroupsError(`Could not load the groups: ${(e as Error).message}`);
      });
    return () => {
      live = false;
    };
  }, [confirmed, groupsAt]);

  // A refresh is over when the service reports a newer group time, or after a timeout.
  React.useEffect(() => {
    if (!refreshFrom) return;
    if (groupsAt !== refreshFrom.at) {
      setRefreshFrom(null);
      return;
    }
    const id = setTimeout(() => {
      setRefreshFrom(null);
      toast.error("The group refresh took too long. Try again.");
    }, GROUPS_REFRESH_TIMEOUT_MS);
    return () => clearTimeout(id);
  }, [refreshFrom, groupsAt]);

  // A request refused because the card was out of date reloads the card before the error is shown.
  const guard = React.useCallback(
    async (fn: () => Promise<void>) => {
      try {
        await fn();
      } catch (e) {
        if (signalFailureIsStale(e)) load();
        throw e;
      }
    },
    [load],
  );

  return {
    mock: MOCK,
    status,
    view,
    loadError,
    viewError,
    uri,
    groups,
    groupsError,
    refreshingGroups: refreshFrom !== null,
    justUnlinked,
    nowMs,
    reload: load,
    startLink: (v) =>
      guard(async () => {
        setRequestedAtMs(Date.now());
        await api.signalStartLink(v);
        setJustUnlinked(false);
        setUri(null);
        setPhase("preparing");
        preparingSince.current = Date.now();
        setNowMs(Date.now());
        load();
      }),
    retryLink: fetchURI,
    cancelLink: () =>
      guard(async () => {
        try {
          await api.signalCancelLink();
        } catch (e) {
          // 409 means no link is pending, which is the state the cancel asks for.
          if (!(e instanceof SignalRequestError && e.status === 409)) throw e;
        } finally {
          // Whatever the answer, the browser stops waiting for a code and shows the real state.
          setPhase("idle");
          setUri(null);
          load();
        }
      }),
    confirm: (digits) =>
      guard(async () => {
        if (!status) throw new Error("The Signal status is not loaded.");
        await api.signalConfirm({ gen: status.gen, last_digits: digits });
        load();
      }),
    unlink: (password) =>
      guard(async () => {
        await api.signalUnlink({ password });
        setJustUnlinked(true);
        setUri(null);
        setPhase("idle");
        toast.success("Unlinked. Also remove the device on the phone.");
        load();
      }),
    refreshGroups: async () => {
      const from = { at: groupsAt };
      await guard(async () => {
        await api.signalRefreshGroups();
        setRefreshFrom(from);
        load();
      });
    },
  };
}

function warningText(w: SignalWarning): string {
  switch (w.kind) {
    case "db_role_missing":
      return "Database role: missing.";
    case "db_role_error":
      return `Database role: error: ${w.detail}`;
    case "db_role_unchecked":
      return "Database role: not checked.";
    case "db_auth_trust":
      return "Database logins: accepts logins without a password.";
    case "db_auth_unchecked":
      return "Database logins: could not be checked.";
    case "db_auth_asserted":
      return "Database logins: asserted by the operator (not checked by ARTEX).";
    case "other":
      return w.text;
  }
}

const STATE_BADGE: Record<string, string> = {
  off: "Off",
  no_service: "No service",
  unknown: "Starting",
  preparing: "Linking",
  preparing_failed: "Linking",
  linking: "Linking",
  linking_stale: "Linking",
  expired: "Expired",
  unlinked: "Not linked",
  linked_unconfirmed: "Needs confirmation",
  linked_confirmed: "Linked",
  error: "Error",
};

function Line({ children }: { children: React.ReactNode }) {
  return <p className="text-sm">{children}</p>;
}

// SignalAccountCard shows the one state of the Signal account the status maps to. The service is
// authoritative: every action reloads the status instead of assuming its result.
export function SignalAccountCard({ signal }: { signal: SignalAccountApi }) {
  const { status, view } = signal;
  const [startOpen, setStartOpen] = React.useState(false);
  const [unlinkOpen, setUnlinkOpen] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  async function run(fn: () => Promise<void>, failure: string): Promise<boolean> {
    setBusy(true);
    try {
      await fn();
      return true;
    } catch (e) {
      toast.error(`${failure}: ${signalErrorText(e)}`);
      return false;
    } finally {
      setBusy(false);
    }
  }

  if (status?.mode === "off") {
    return (
      <Card className="gap-3">
        <CardHeader>
          <CardTitle className="text-base">Signal account</CardTitle>
        </CardHeader>
        <CardContent>
          <Line>{SIGNAL_OFF_TEXT}</Line>
        </CardContent>
      </Card>
    );
  }

  const state = view?.state;
  const kind = signal.mock ? "no_service" : (state?.kind ?? null);

  function body(): React.ReactNode {
    if (signal.mock) return noService();
    if (!status) {
      if (signal.loadError) {
        return (
          <div className="flex flex-wrap items-center gap-3">
            <Line>{signal.loadError}</Line>
            <Button size="sm" variant="outline" onClick={signal.reload}>
              Retry
            </Button>
          </div>
        );
      }
      return (
        <p className="text-muted-foreground flex items-center gap-2 text-sm">
          <Spinner /> Loading
        </p>
      );
    }
    if (!state)
      return <Line>The status reported by the server could not be read ({signal.viewError}). Reload the page.</Line>;
    switch (state.kind) {
      case "off":
        return null;
      case "no_service":
        return noService();
      case "unknown":
        return <Line>The Signal service is starting (no state reported yet).</Line>;
      case "preparing":
        return (
          <div className="flex flex-wrap items-center gap-3">
            <p className="text-muted-foreground flex items-center gap-2 text-sm">
              <Spinner /> Preparing the link...
            </p>
            <Button size="sm" variant="outline" disabled={busy} onClick={() => run(signal.cancelLink, "Cancel failed")}>
              Cancel
            </Button>
          </div>
        );
      case "preparing_failed":
        return (
          <div className="flex flex-wrap items-center gap-3">
            <Line>Could not load the link code.</Line>
            {state.lastError && <Line>{state.lastError}</Line>}
            <Button size="sm" variant="outline" onClick={signal.retryLink}>
              Retry
            </Button>
            <Button size="sm" variant="outline" disabled={busy} onClick={() => run(signal.cancelLink, "Cancel failed")}>
              Cancel
            </Button>
          </div>
        );
      case "linking":
      case "linking_stale":
        if (!signal.uri) {
          return (
            <p className="text-muted-foreground flex items-center gap-2 text-sm">
              <Spinner /> Preparing the link...
            </p>
          );
        }
        return (
          <SignalLinkPanel
            uri={signal.uri.uri}
            secondsLeft={state.secondsLeft}
            stale={state.kind === "linking_stale"}
            cancelling={busy}
            onCancel={() => run(signal.cancelLink, "Cancel failed")}
          />
        );
      case "expired":
        return (
          <div className="flex flex-wrap items-center gap-3">
            <Line>Link expired. Start again.</Line>
            <Button
              size="sm"
              disabled={busy}
              onClick={() => run(signal.cancelLink, "Reset failed").then((ok) => ok && setStartOpen(true))}
            >
              Link account
            </Button>
          </div>
        );
      case "unlinked":
        return (
          <div className="grid gap-3">
            {signal.justUnlinked || state.removedOnSignal ? (
              <Line>
                {state.removedOnSignal === "removed"
                  ? "Unlinked. The device was also removed on Signal's side."
                  : "Unlinked on this server only. Remove the device (the name you chose when linking) under Linked devices in Signal on the phone."}
              </Line>
            ) : null}
            {state.lastError && <Line>{state.lastError}</Line>}
            <Line>
              No account linked. Use a dedicated Signal account: the linked device can read all messages of that
              account.
            </Line>
            <div>
              <Button onClick={() => setStartOpen(true)}>Link account</Button>
            </div>
          </div>
        );
      case "linked_unconfirmed":
        return (
          <SignalConfirmForm
            numberHint={state.numberHint}
            onConfirm={signal.confirm}
            onUnlink={() => setUnlinkOpen(true)}
          />
        );
      case "linked_confirmed":
        return (
          <div className="grid gap-3">
            <Line>
              Linked account: <span className="font-mono">{state.numberHint}</span>
              {" · "}Groups: {state.groupsCount} (updated {relativeAge(state.groupsAt, signal.nowMs)})
            </Line>
            {signal.groupsError && <p className="text-destructive text-sm">{signal.groupsError}</p>}
            <div className="flex flex-wrap gap-2">
              <Button
                variant="outline"
                disabled={signal.refreshingGroups || busy}
                onClick={() => run(signal.refreshGroups, "Refresh failed")}
              >
                {signal.refreshingGroups && <Spinner />} Refresh groups
              </Button>
              <Button variant="outline" onClick={() => setUnlinkOpen(true)}>
                Unlink
              </Button>
            </div>
          </div>
        );
      case "error":
        return (
          <div className="grid gap-3">
            <Line>
              {state.lastError ?? "The account is in an error state."}
              {state.lastErrorAt ? ` (since ${new Date(state.lastErrorAt).toLocaleTimeString()})` : ""}
            </Line>
            <Line>To start over, unlink: this deletes the local account data. Linking is not possible until then.</Line>
            <div>
              <Button variant="destructive" onClick={() => setUnlinkOpen(true)}>
                Unlink and reset
              </Button>
            </div>
          </div>
        );
    }
  }

  function noService() {
    return (
      <Line>
        The Signal service is not connected. Start the Signal override of the deployment (see the deployment README).
      </Line>
    );
  }

  const warnings = view?.warnings ?? [];
  const serviceLine =
    status?.sidecar.alive && !signal.mock
      ? signalServiceLine(status.sidecar.version, status.sidecar.signal_cli_version)
      : "Service: not connected";

  return (
    <Card className="gap-3">
      <CardHeader>
        <div className="flex items-center gap-2">
          <CardTitle className="text-base">Signal account</CardTitle>
          {kind && <Badge variant="outline">{STATE_BADGE[kind]}</Badge>}
        </div>
        {(status !== null || signal.mock) && <p className="text-muted-foreground text-xs">{serviceLine}</p>}
      </CardHeader>
      <CardContent className="grid gap-3">
        {body()}
        {!signal.mock && status && signal.loadError && <p className="text-destructive text-xs">{signal.loadError}</p>}
        {warnings.length > 0 && (
          <ul className="text-destructive list-disc pl-5 text-xs">
            {warnings.map((w) => (
              <li key={warningText(w)}>{warningText(w)}</li>
            ))}
          </ul>
        )}
      </CardContent>
      <SignalLinkStartDialog open={startOpen} onOpenChange={setStartOpen} onStart={signal.startLink} />
      <SignalUnlinkDialog open={unlinkOpen} onOpenChange={setUnlinkOpen} onUnlink={signal.unlink} />
    </Card>
  );
}
