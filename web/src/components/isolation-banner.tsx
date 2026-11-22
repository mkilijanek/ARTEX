"use client";

import { useEffect, useState } from "react";

import { ChevronDown, ChevronUp, TriangleAlert, X } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { ISOLATION_DOCS_URL, isolationBannerKind, isolationBannerText } from "@/lib/isolation-banner";
import type { ToolIsolation } from "@/lib/types";
import { cn } from "@/lib/utils";

const STORE_KEY = "artex_isolation_banner";

// Per browser session (sessionStorage): the collapsed or dismissed choice is forgotten when the tab closes.
function readFlag(kind: string): boolean {
  try {
    return window.sessionStorage.getItem(`${STORE_KEY}_${kind}`) === "1";
  } catch {
    return false;
  }
}

function writeFlag(kind: string, on: boolean) {
  try {
    window.sessionStorage.setItem(`${STORE_KEY}_${kind}`, on ? "1" : "0");
  } catch (e) {
    console.warn("isolation banner: could not store the choice", e);
  }
}

export function IsolationBanner({ isolation }: { isolation: ToolIsolation | null | undefined }) {
  const kind = isolationBannerKind(isolation);
  const [hidden, setHidden] = useState(false);
  useEffect(() => setHidden(readFlag(kind)), [kind]);

  if (!isolation || kind === "none") return null;
  const dismissible = kind === "notice" || kind === "unknown";
  if (dismissible && hidden) return null;
  const { title, body } = isolationBannerText(isolation);
  const red = kind === "unavailable" || kind === "degraded";
  const collapsed = kind === "degraded" && hidden;

  return (
    <Alert
      data-testid="isolation-banner"
      data-kind={kind}
      className={cn(
        "mb-4 pr-10",
        red
          ? "border-red-500/60 bg-red-50 text-red-900 dark:border-red-500/50 dark:bg-red-950/40 dark:text-red-100"
          : "border-yellow-500/60 bg-yellow-50 text-yellow-900 dark:border-yellow-500/50 dark:bg-yellow-950/40 dark:text-yellow-100",
        kind === "unavailable" && "border-2",
      )}
    >
      <TriangleAlert />
      <AlertTitle>{title}</AlertTitle>
      {!collapsed && (
        <AlertDescription className="text-current/90">
          {body}{" "}
          <a href={ISOLATION_DOCS_URL} target="_blank" rel="noreferrer">
            Tool isolation documentation
          </a>
        </AlertDescription>
      )}
      {kind === "degraded" && (
        <Button
          variant="ghost"
          size="icon"
          className="absolute top-1 right-1 size-7"
          aria-label={collapsed ? "Expand the isolation warning" : "Collapse the isolation warning"}
          onClick={() => {
            writeFlag(kind, !hidden);
            setHidden(!hidden);
          }}
        >
          {collapsed ? <ChevronDown /> : <ChevronUp />}
        </Button>
      )}
      {dismissible && (
        <Button
          variant="ghost"
          size="icon"
          className="absolute top-1 right-1 size-7"
          aria-label="Dismiss the isolation notice"
          onClick={() => {
            writeFlag(kind, true);
            setHidden(true);
          }}
        >
          <X />
        </Button>
      )}
    </Alert>
  );
}
