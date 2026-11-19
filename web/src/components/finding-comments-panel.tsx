"use client";

import * as React from "react";

import { MessageSquarePlusIcon } from "lucide-react";
import { toast } from "sonner";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import {
  commentErrorMessage,
  countCodePoints,
  DEFAULT_MAX_COMMENT_RUNES,
  mergeComments,
  validateComment,
} from "@/lib/finding-comments";
import type { FindingComment } from "@/lib/types";
import { cn } from "@/lib/utils";

function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("zh-CN");
}

// Author kind and subject come from the verified session on the server. Plain text only.
function authorLabel(author: FindingComment["author"]) {
  return author.kind === "operator"
    ? `${author.subject} · shared operator account`
    : `${author.subject} (${author.kind})`;
}

// Analyst notes on a finding. Every comment field is rendered as a React text node inside a
// whitespace-pre-wrap element: no HTML, no formatting syntax, no link detection.
export function FindingCommentsPanel({
  findingId,
  contextTask,
  readOnly = false,
  className,
}: {
  findingId: string;
  contextTask?: string;
  readOnly?: boolean;
  className?: string;
}) {
  const textareaId = React.useId();
  const draftMessageId = `${textareaId}-msg`;
  const [items, setItems] = React.useState<FindingComment[] | null>(null);
  const [cursor, setCursor] = React.useState("");
  const [maxRunes, setMaxRunes] = React.useState(DEFAULT_MAX_COMMENT_RUNES);
  const [error, setError] = React.useState("");
  const [moreError, setMoreError] = React.useState("");
  const [loadingMore, setLoadingMore] = React.useState(false);
  const [draft, setDraft] = React.useState("");
  const [submitting, setSubmitting] = React.useState(false);
  const [submitError, setSubmitError] = React.useState("");
  const requestSeq = React.useRef(0);
  const submitLock = React.useRef(false);

  const load = React.useCallback(async () => {
    const seq = ++requestSeq.current;
    setError("");
    setMoreError("");
    setLoadingMore(false);
    try {
      const page = await api.findingComments(findingId, { contextTask });
      if (seq !== requestSeq.current) return;
      setItems(page.comments);
      setCursor(page.next_cursor);
      setMaxRunes(page.max_body_runes > 0 ? page.max_body_runes : DEFAULT_MAX_COMMENT_RUNES);
    } catch (e) {
      if (seq === requestSeq.current) setError(commentErrorMessage(e));
    }
  }, [findingId, contextTask]);

  React.useEffect(() => {
    void load();
    return () => {
      requestSeq.current++;
    };
  }, [load]);

  async function loadMore() {
    if (!cursor || loadingMore) return;
    const seq = requestSeq.current;
    setLoadingMore(true);
    setMoreError("");
    try {
      const page = await api.findingComments(findingId, { cursor, contextTask });
      if (seq !== requestSeq.current) return;
      setItems((current) => mergeComments(current ?? [], page.comments));
      setCursor(page.next_cursor);
      setMaxRunes(page.max_body_runes > 0 ? page.max_body_runes : DEFAULT_MAX_COMMENT_RUNES);
    } catch (e) {
      if (seq === requestSeq.current) setMoreError(commentErrorMessage(e));
    } finally {
      if (seq === requestSeq.current) setLoadingMore(false);
    }
  }

  const check = validateComment(draft, maxRunes);
  const count = check.ok || draft.trim() !== "" ? check.length : countCodePoints(draft);
  const canSubmit = check.ok && !submitting;

  async function submit() {
    if (submitLock.current || !check.ok) return;
    submitLock.current = true;
    setSubmitting(true);
    setSubmitError("");
    try {
      const result = await api.addFindingComment(findingId, check.body, contextTask);
      setItems((current) => mergeComments(current ?? [], [result.comment]));
      setDraft("");
      toast.success("Comment added");
    } catch (e) {
      // Keep the draft; the server's message is shown inline (never the comment text).
      setSubmitError(commentErrorMessage(e));
    } finally {
      submitLock.current = false;
      setSubmitting(false);
    }
  }

  return (
    <Card className={cn("min-w-0", className)}>
      <CardHeader>
        <CardTitle className="text-sm">Comments</CardTitle>
        <CardDescription>Notes for analysts. Not sent to agents and not included in exports.</CardDescription>
      </CardHeader>
      <CardContent className="flex min-w-0 flex-col gap-3 lg:min-h-0 lg:flex-1">
        {/* From lg up a height-bounded parent makes this area scroll; the input below stays in view. */}
        <div
          data-testid="comments-list"
          className="flex min-w-0 flex-col gap-3 lg:min-h-16 lg:flex-1 lg:overflow-y-auto"
        >
          {error ? (
            <Alert variant="destructive">
              <AlertDescription>
                Failed to load comments: {error}
                <Button variant="outline" size="sm" onClick={() => void load()}>
                  Retry
                </Button>
              </AlertDescription>
            </Alert>
          ) : null}
          {!error && items === null ? <Skeleton className="h-16 w-full" /> : null}
          {!error && items?.length === 0 ? (
            <Empty>
              <EmptyHeader>
                <EmptyTitle>No comments yet</EmptyTitle>
                <EmptyDescription>Add a note for the next analyst who opens this finding.</EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : null}
          {!error && items
            ? items.map((item) => (
                <div key={item.id} className="flex min-w-0 flex-col gap-1.5 rounded-lg border p-3">
                  <div className="flex flex-wrap items-center gap-x-2 text-muted-foreground text-xs">
                    <span className="break-all">{authorLabel(item.author)}</span>
                    <span>·</span>
                    <span>{fmtTime(item.created_at)}</span>
                  </div>
                  <p className="text-sm leading-relaxed whitespace-pre-wrap break-words [overflow-wrap:anywhere]">
                    {item.body}
                  </p>
                </div>
              ))
            : null}
          {!error && cursor ? (
            <div className="flex flex-wrap items-center gap-2">
              <Button variant="outline" size="sm" disabled={loadingMore} onClick={() => void loadMore()}>
                {loadingMore ? <Spinner data-icon="inline-start" aria-hidden="true" /> : null}
                Load more
              </Button>
              {moreError ? (
                <span className="text-destructive text-xs">
                  Failed to load more comments: {moreError}{" "}
                  <button type="button" className="underline" onClick={() => void load()}>
                    Reload
                  </button>
                </span>
              ) : null}
            </div>
          ) : null}
        </div>
        {readOnly ? (
          <p className="text-muted-foreground text-sm">
            This finding is shown through an inherited task. Comments are read-only here; add them from the task that
            owns the finding.
          </p>
        ) : (
          <div className="flex min-w-0 flex-col gap-2">
            <label htmlFor={textareaId} className="sr-only">
              New comment
            </label>
            <Textarea
              id={textareaId}
              value={draft}
              rows={3}
              disabled={submitting}
              aria-invalid={!check.ok && draft.trim() !== "" ? true : undefined}
              aria-describedby={!check.ok && draft.trim() !== "" ? draftMessageId : undefined}
              placeholder="Add a comment (plain text). Ctrl/Cmd+Enter to submit."
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
                  e.preventDefault();
                  void submit();
                }
              }}
            />
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span
                className={`text-xs ${count > maxRunes ? "text-destructive" : "text-muted-foreground"}`}
                aria-live="polite"
              >
                {count} / {maxRunes}
              </span>
              <Button size="sm" onClick={() => void submit()} disabled={!canSubmit}>
                {submitting ? (
                  <Spinner data-icon="inline-start" aria-hidden="true" />
                ) : (
                  <MessageSquarePlusIcon data-icon="inline-start" />
                )}
                Add comment
              </Button>
            </div>
            {!check.ok && draft.trim() !== "" ? (
              <p id={draftMessageId} className="text-destructive text-xs" role="status">
                {check.message}
              </p>
            ) : null}
            {submitError ? (
              <Alert variant="destructive">
                <AlertDescription>Failed to add comment: {submitError}</AlertDescription>
              </Alert>
            ) : null}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
