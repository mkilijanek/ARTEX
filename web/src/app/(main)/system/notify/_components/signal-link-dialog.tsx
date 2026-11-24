"use client";

import * as React from "react";

import { QRCodeSVG } from "qrcode.react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { SignalRequestError, signalFailureKind } from "@/lib/signal-account";

import { formatCountdown, isLastDigits } from "./channel-fields";

// signalErrorText words a failed Signal request. Anything that is not a Signal route answer (a network
// failure, for example) keeps its own message.
export function signalErrorText(e: unknown): string {
  if (!(e instanceof SignalRequestError)) return e instanceof Error ? e.message : "The request failed.";
  switch (signalFailureKind(e)) {
    case "password_incorrect":
      return "Current password is wrong.";
    case "number_mismatch":
      return "Those digits do not match the linked account. Check the phone you scanned with, or unlink.";
    case "link_pending":
      return "A link is already in progress. Cancel it first.";
    case "already_linked":
      return "An account is already linked.";
    case "unlink_first":
      return "The service is in an error state. Unlink first, then link again.";
    case "not_ready":
      return "The Signal service is still starting. Try again in a moment.";
    case "conflict":
      return "The account changed meanwhile; the card has been reloaded.";
    case "throttled":
      return e.retryAfterS === null
        ? "Too many attempts. Try again later."
        : `Too many attempts. Try again in ${e.retryAfterS} s.`;
    case "unavailable":
      return "The Signal service is not responding.";
    default:
      return `The request failed (${e.status}).`;
  }
}

// A request failure that means the card shown is out of date, so it should reload.
export function signalFailureIsStale(e: unknown): boolean {
  if (!(e instanceof SignalRequestError)) return false;
  const k = signalFailureKind(e);
  return (
    k === "conflict" || k === "link_pending" || k === "already_linked" || k === "unlink_first" || k === "not_ready"
  );
}

// The password a user types stays in this component's state only, and is dropped when the dialog closes.
export function SignalLinkStartDialog({
  open,
  onOpenChange,
  onStart,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onStart: (v: { password: string; device_name: string; acknowledge: boolean }) => Promise<void>;
}) {
  const [password, setPassword] = React.useState("");
  const [deviceName, setDeviceName] = React.useState("ARTEX");
  const [ack, setAck] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (!open) {
      setPassword("");
      setDeviceName("ARTEX");
      setAck(false);
      setError(null);
    }
  }, [open]);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onStart({ password, device_name: deviceName.trim(), acknowledge: ack });
      onOpenChange(false);
    } catch (err) {
      setError(signalErrorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4">
          <DialogHeader>
            <DialogTitle>Link a Signal account</DialogTitle>
            <DialogDescription>
              ARTEX becomes a linked device of one Signal account. Scan the code with Signal on the phone of that
              account.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="sig-device">Device name</Label>
            <Input id="sig-device" value={deviceName} maxLength={50} onChange={(e) => setDeviceName(e.target.value)} />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="sig-link-pw">Your current password</Label>
            <Input
              id="sig-link-pw"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
          <div className="flex items-start gap-2 text-sm">
            <Checkbox id="sig-ack" checked={ack} onCheckedChange={(v) => setAck(v === true)} className="mt-0.5" />
            <Label htmlFor="sig-ack" className="font-normal leading-snug">
              I understand that the linked device can read all messages of this Signal account, and that I use an
              account meant for this purpose.
            </Label>
          </div>
          {error && (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Close
            </Button>
            <Button type="submit" disabled={busy || !ack || password === "" || deviceName.trim() === ""}>
              {busy && <Spinner />} Start
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function SignalUnlinkDialog({
  open,
  onOpenChange,
  onUnlink,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onUnlink: (password: string) => Promise<void>;
}) {
  const [password, setPassword] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (!open) {
      setPassword("");
      setError(null);
    }
  }, [open]);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onUnlink(password);
      onOpenChange(false);
    } catch (err) {
      setError(signalErrorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4">
          <DialogHeader>
            <DialogTitle>Unlink the Signal account</DialogTitle>
            <DialogDescription>
              This deletes the account data stored by ARTEX and stops all Signal notifications. It does not remove the
              device on the phone: do that under Linked devices in Signal.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-2">
            <Label htmlFor="sig-unlink-pw">Your current password</Label>
            <Input
              id="sig-unlink-pw"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
          {error && (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Close
            </Button>
            <Button type="submit" variant="destructive" disabled={busy || password === ""}>
              {busy && <Spinner />} Unlink
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// SignalLinkPanel shows the link code while it is valid. The code lives only in memory: it is drawn
// here as an SVG in the browser and is never written to storage, the console or a request.
export function SignalLinkPanel({
  uri,
  secondsLeft,
  stale,
  onCancel,
  cancelling,
}: {
  uri: string;
  secondsLeft: number;
  stale: boolean;
  onCancel: () => void;
  cancelling: boolean;
}) {
  const [showText, setShowText] = React.useState(false);
  return (
    <div className="flex flex-col gap-4 sm:flex-row">
      <div
        role="img"
        aria-label="Link code to scan with Signal. The same code is available as text under Show link text."
        className={`shrink-0 self-start rounded-md border bg-white p-1 ${stale ? "opacity-30 grayscale" : ""}`}
      >
        <QRCodeSVG value={uri} size={224} level="M" marginSize={4} bgColor="#ffffff" fgColor="#000000" />
      </div>
      <div className="grid content-start gap-2 text-sm">
        {stale ? (
          <p className="text-destructive">
            The Signal service stopped responding while linking. The code may no longer work.
          </p>
        ) : (
          <>
            <p>Scan this code with Signal on the phone:</p>
            <p className="text-muted-foreground">Settings, Linked devices, Link a new device.</p>
          </>
        )}
        <p>
          Expires in <span className="font-mono tabular-nums">{formatCountdown(secondsLeft)}</span>
        </p>
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="outline" onClick={() => setShowText((v) => !v)}>
            {showText ? "Hide link text" : "Show link text"}
          </Button>
          <Button size="sm" variant="outline" onClick={onCancel} disabled={cancelling}>
            Cancel
          </Button>
        </div>
        {showText && (
          <Input
            readOnly
            value={uri}
            aria-label="Link text"
            className="font-mono text-xs"
            onFocus={(e) => e.currentTarget.select()}
          />
        )}
      </div>
    </div>
  );
}

// SignalConfirmForm asks for the last six digits of the linked number. The error text from a refused
// attempt stays until the next try; a throttled attempt counts down and disables the button meanwhile.
export function SignalConfirmForm({
  numberHint,
  onConfirm,
  onUnlink,
}: {
  numberHint: string;
  onConfirm: (digits: string) => Promise<void>;
  onUnlink: () => void;
}) {
  const [digits, setDigits] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [waitUntil, setWaitUntil] = React.useState<number | null>(null);
  const [now, setNow] = React.useState(() => Date.now());

  React.useEffect(() => {
    if (waitUntil === null) return;
    const id = setInterval(() => setNow(Date.now()), 500);
    return () => clearInterval(id);
  }, [waitUntil]);

  const waitLeft = waitUntil === null ? 0 : Math.max(0, Math.ceil((waitUntil - now) / 1000));

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onConfirm(digits);
      setDigits("");
    } catch (err) {
      if (err instanceof SignalRequestError && err.retryAfterS !== null && signalFailureKind(err) === "throttled") {
        setNow(Date.now());
        setWaitUntil(Date.now() + err.retryAfterS * 1000);
      }
      setError(signalErrorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form onSubmit={submit} className="grid gap-3 text-sm">
      <p>
        An account is linked: <span className="font-mono">{numberHint}</span>. To confirm it is the account you meant,
        type the last 6 digits of its phone number.
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <Label htmlFor="sig-digits" className="sr-only">
          Last 6 digits
        </Label>
        <Input
          id="sig-digits"
          className="w-32 font-mono"
          inputMode="numeric"
          autoComplete="off"
          maxLength={6}
          placeholder="000000"
          value={digits}
          onChange={(e) => setDigits(e.target.value.replace(/[^0-9]/g, ""))}
        />
        <Button type="submit" disabled={busy || waitLeft > 0 || !isLastDigits(digits)}>
          {busy && <Spinner />} Confirm
        </Button>
        <Button type="button" variant="outline" onClick={onUnlink}>
          Unlink
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-destructive">
          {waitLeft > 0 ? `Too many attempts. Try again in ${waitLeft} s.` : error}
        </p>
      )}
      <p className="text-muted-foreground text-xs">Notifications are not sent until you confirm.</p>
    </form>
  );
}
