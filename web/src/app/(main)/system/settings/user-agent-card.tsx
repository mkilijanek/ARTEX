"use client";

import * as React from "react";

import { GlobeIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import type { Settings } from "@/lib/types";
import { effectiveUserAgent, UA_COMPONENTS, type UAComponent } from "@/lib/user-agent";

// Library components whose User-Agent is fixed by Norma; shown greyed out, never sent or stored as active.
const LOCKED_COMPONENTS = ["WebFetch", "WebSearch"] as const;

function Hint({ value }: { value: string }) {
  return (
    <code className="block max-w-full truncate font-mono text-[11px] text-muted-foreground" title={value}>
      {value}
    </code>
  );
}

/** Advanced settings: the User-Agent family, its master switch, an optional custom value and the per-component choice. */
export function UserAgentCard({
  settings,
  onApplied,
}: {
  settings: Settings | null;
  onApplied: (s: Settings) => void;
}) {
  const [hidden, setHidden] = React.useState(true);
  const [custom, setCustom] = React.useState("");
  const [components, setComponents] = React.useState<string[]>([...UA_COMPONENTS]);
  const [saving, setSaving] = React.useState(false);
  const loaded = settings !== null;

  React.useEffect(() => {
    if (!settings) return;
    setHidden(settings.user_agent_hidden !== false);
    setCustom(settings.user_agent_custom ?? "");
    setComponents(settings.user_agent_components ?? [...UA_COMPONENTS]);
  }, [settings]);

  const version = settings?.user_agent_version ?? "dev";
  const inputs = { hidden, custom, components, version };

  const save = (patch: Partial<Settings>, okMessage: string, revert?: () => void) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        onApplied(s);
        toast.success(okMessage);
      })
      .catch((e) => {
        revert?.();
        toast.error(`Save failed: ${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleHidden = (v: boolean) => {
    setHidden(v); // optimistic
    save({ user_agent_hidden: v }, v ? "ARTEX user agent hidden" : "ARTEX user agent shown", () => setHidden(!v));
  };

  const toggleComponent = (name: string, on: boolean) =>
    setComponents((cur) => (on ? [...cur, name] : cur.filter((c) => c !== name)));

  const saveDetails = () =>
    save({ user_agent_custom: custom.trim(), user_agent_components: components }, "User-Agent settings saved");

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <GlobeIcon className="size-4" />
          Advanced settings · User-Agent
        </CardTitle>
        <CardDescription>
          When the switch is on, ARTEX sends no identifying User-Agent. When off, outbound requests of the checked
          components carry the custom value, or <code className="font-mono text-xs">ARTEX/&lt;version&gt;</code> with
          the component name. A User-Agent set on a provider profile, MCP server, notify channel or custom tool
          overrides it. Applies to new requests without a restart.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="flex items-center justify-between gap-4">
          <Label htmlFor="ua-hidden" className="text-sm font-normal text-muted-foreground">
            Hide ARTEX user agent
          </Label>
          <Switch id="ua-hidden" checked={hidden} disabled={!loaded || saving} onCheckedChange={toggleHidden} />
        </div>

        <div className="flex flex-col gap-2">
          <Label htmlFor="ua-custom" className="text-sm font-normal text-muted-foreground">
            Custom User-Agent (optional, used for all checked components)
          </Label>
          <Input
            id="ua-custom"
            className="font-mono text-sm"
            autoComplete="off"
            maxLength={256}
            placeholder="empty = ARTEX/<version> (+component)"
            value={custom}
            disabled={!loaded || saving || hidden}
            onChange={(e) => setCustom(e.target.value)}
          />
        </div>

        <div className="flex flex-col gap-3">
          {UA_COMPONENTS.map((name: UAComponent) => (
            <div key={name} className="flex items-start gap-2">
              <Checkbox
                id={`ua-c-${name}`}
                className="mt-0.5"
                checked={components.includes(name)}
                disabled={!loaded || saving || hidden}
                onCheckedChange={(v) => toggleComponent(name, v === true)}
              />
              <div className="min-w-0 flex-1">
                <Label htmlFor={`ua-c-${name}`} className="text-sm font-normal">
                  {name}
                </Label>
                <Hint value={effectiveUserAgent(inputs, name)} />
              </div>
            </div>
          ))}
          {LOCKED_COMPONENTS.map((name) => (
            <div key={name} className="flex items-start gap-2 opacity-60" data-testid={`ua-locked-${name}`}>
              <Checkbox id={`ua-c-${name}`} className="mt-0.5" checked={false} disabled />
              <div className="min-w-0 flex-1">
                <Label htmlFor={`ua-c-${name}`} className="text-sm font-normal">
                  {name}{" "}
                  <span className="text-xs text-muted-foreground">
                    controlled by the Norma library; not configurable yet
                  </span>
                </Label>
                <Hint value={settings?.user_agent_effective?.[name] ?? ""} />
              </div>
            </div>
          ))}
        </div>

        <div className="flex justify-end">
          <Button onClick={saveDetails} disabled={!loaded || saving || hidden}>
            Save
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
