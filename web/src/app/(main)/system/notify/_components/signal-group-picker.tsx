"use client";

import * as React from "react";

import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";

import { cleanGroupName, groupRecipient, groupSelection, recipientsToText } from "./channel-fields";
import { SIGNAL_OFF_TEXT, type SignalAccountApi } from "./signal-account-card";
import { signalErrorText } from "./signal-link-dialog";

// SignalGroupPicker chooses the one group a Signal channel sends to, from the groups of the linked
// account. Manual entry of group.<id> stays available for a value that is not in the list and when no
// list exists. Names are other people's text: cleaned and shown as text, always with the masked id.
export function SignalGroupPicker({
  signal,
  value,
  onChange,
}: {
  signal: SignalAccountApi;
  value: unknown;
  onChange: (v: string) => void;
}) {
  const groups = signal.groups;
  const selection = groupSelection(groups, value);
  // The user's own choice between the list and manual entry sticks; until then the value decides.
  const [forced, setForced] = React.useState<boolean | null>(null);
  const manual = forced ?? selection.kind === "manual";
  const off = signal.status?.mode === "off";

  async function refresh() {
    try {
      await signal.refreshGroups();
    } catch (e) {
      toast.error(`Refresh failed: ${signalErrorText(e)}`);
    }
  }

  return (
    <div className="grid gap-2">
      <Label htmlFor={manual ? "n-cfg-recipients" : undefined}>Group</Label>
      {off && <p className="text-muted-foreground text-xs">{SIGNAL_OFF_TEXT}</p>}
      {!manual && groups ? (
        <div className="flex items-center gap-2">
          <Select value={selection.kind === "selected" ? selection.recipient : ""} onValueChange={(v) => onChange(v)}>
            <SelectTrigger className="min-w-0 flex-1" aria-label="Group">
              <SelectValue placeholder="Choose a group" />
            </SelectTrigger>
            <SelectContent>
              {groups.map((g) => {
                const name = cleanGroupName(g.name);
                return (
                  <SelectItem key={g.id} value={groupRecipient(g.id)}>
                    {name || "(unnamed group)"}{" "}
                    <span className="text-muted-foreground font-mono text-xs">{g.masked}</span>
                  </SelectItem>
                );
              })}
            </SelectContent>
          </Select>
          <Button type="button" variant="outline" disabled={signal.refreshingGroups} onClick={refresh}>
            {signal.refreshingGroups && <Spinner />} Refresh
          </Button>
        </div>
      ) : (
        <Input
          id="n-cfg-recipients"
          className="font-mono"
          placeholder="group.<id>"
          value={recipientsToText(value)}
          onChange={(e) => {
            setForced(true);
            onChange(e.target.value);
          }}
        />
      )}
      <div className="flex flex-wrap items-center gap-2">
        {!manual && groups && (
          <Button type="button" size="sm" variant="ghost" onClick={() => setForced(true)}>
            Enter id manually
          </Button>
        )}
        {manual && selection.kind !== "manual" && (
          <Button type="button" size="sm" variant="ghost" onClick={() => setForced(false)}>
            Choose from the list
          </Button>
        )}
      </div>
      <p className="text-muted-foreground text-xs">
        Exactly one group.&lt;id&gt;. The masked id next to a name tells groups with the same name apart.
      </p>
    </div>
  );
}
