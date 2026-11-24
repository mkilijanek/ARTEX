// Channel field table and config-value parsing helpers.
//
// This lives apart from the page because it is **data**, not a view: it describes which fields each channel has,
// which control each field uses, and the two-way conversion between form text and config values (JSON).
// In its own file, adding a channel only touches this file — the page itself needs no changes.
// Display names and blurbs for channel types. Kept in the frontend because they only affect copy; the backend does not need to know.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Feishu",
  wecom: "WeCom",
  webhook: "Generic Webhook",
  telegram: "Telegram",
  signal: "Signal",
  email: "Email",
};

// Config field definitions per channel.
//
// A frontend field table is kept here deliberately instead of having the backend send down a schema: the backend only does
// Validate (required/format), while the UI needs layout and control types — not the same concern.
// The single coupling point is secret_keys — which fields render as password inputs comes from the backend,
// because only the channel implementation knows which values are credentials (WeCom's entire Webhook is the credential,
// whereas DingTalk's is just one secret). A missing entry here for a new channel only blanks the form;
// it does not fail silently (hasFields below surfaces it).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "Signing secret",
      kind: "password",
      help: "Fill in when the robot's security settings use 'Sign'; leave empty for 'Custom keywords' or when no security setting is enabled",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "Signature verification secret", kind: "password", help: "Fill in when the robot enables 'Signature verification'; otherwise leave empty" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook URL",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "Target URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "HTTP method",
      kind: "select",
      options: [
        { value: "POST", label: "POST (with body)" },
        { value: "PUT", label: "PUT (with body)" },
        { value: "PATCH", label: "PATCH (with body)" },
        { value: "GET", label: "GET (no body)" },
      ],
    },
    { key: "headers", label: "Custom headers", kind: "kv", help: "One KEY=VALUE per line, e.g. Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "Body template",
      kind: "textarea",
      help:
        "Leave empty to use the built-in default template. Variables: {{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}, " +
        "plus .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel under range .Items. " +
        "Use {{json .Xxx}} instead of {{.Xxx}} to insert strings, or quotes in titles will break the JSON.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API URL",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "Leave empty for the official URL; fill in when proxying a self-hosted Bot API",
    },
  ],
  signal: [
    {
      key: "recipient_type",
      label: "Recipient type",
      kind: "select",
      help: "Recommended for team alerts: members are managed in Signal and ARTEX stores no personal phone numbers. Pick a group of the linked account, or enter group.<id> by hand",
      options: [
        { value: "group", label: "Group (recommended)" },
        { value: "numbers", label: "Phone numbers" },
      ],
    },
    {
      key: "recipients",
      label: "Recipients",
      kind: "list",
      help: "For a group: exactly one group.<id>. For phone numbers: one or more numbers in international format, e.g. +15555550101.",
    },
  ],
  email: [
    { key: "host", label: "SMTP server", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "Port",
      kind: "number",
      placeholder: "587",
      help: "587 uses STARTTLS; for 465 turn 'Implicit TLS' on",
    },
    { key: "username", label: "Account", kind: "text" },
    { key: "password", label: "Password / auth code", kind: "password" },
    { key: "from", label: "From", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "Recipients", kind: "list", help: "Separate multiple addresses with commas" },
    { key: "tls", label: "Implicit TLS", kind: "switch", help: "On for port 465; keep off for 587 (STARTTLS is automatic)" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "All severities" },
  { value: "low", label: "Low and above" },
  { value: "medium", label: "Medium and above" },
  { value: "high", label: "High and above" },
  { value: "critical", label: "Critical only" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

// Per-kind starting config for a new form or a kind switch. Signal starts with
// group recipients: the safer choice, since no personal phone numbers are stored.
export const defaultConfig = (kind: string): Record<string, unknown> =>
  kind === "signal" ? { recipient_type: "group" } : {};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: defaultConfig(kind),
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV parses a "KEY=VALUE per line" textarea.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs parses a comma/whitespace-separated id list.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords parses a line/comma-separated keyword list (vulnerability type names may contain spaces, hence splitting on lines or commas).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

// ---- Signal helpers (pure, so they can be tested without a browser) ----

// A group as the picker needs it. Structurally the same as SignalGroup in lib/types.
export interface GroupOption {
  id: string;
  masked: string;
  name: string;
}

// formatCountdown prints whole seconds as mm:ss. Anything negative or not a number prints 00:00.
export function formatCountdown(seconds: number): string {
  const s = Number.isFinite(seconds) && seconds > 0 ? Math.floor(seconds) : 0;
  const mm = String(Math.floor(s / 60)).padStart(2, "0");
  const ss = String(s % 60).padStart(2, "0");
  return `${mm}:${ss}`;
}

// isLastDigits reports whether the text is exactly the six ASCII digits the confirmation asks for.
export function isLastDigits(text: string): boolean {
  return /^[0-9]{6}$/.test(text);
}

const GROUP_NAME_MAX = 64;

// cleanGroupName makes a group name safe to show: white space is collapsed, control and format
// characters are dropped and the result is cut. Group names come from other people, so the
// result is only ever rendered as text.
export function cleanGroupName(name: string): string {
  const cleaned = name
    .replace(/\s+/gu, " ")
    .replace(/[\p{Cc}\p{Cf}]/gu, "")
    .replace(/ {2,}/g, " ")
    .trim();
  const chars = Array.from(cleaned);
  if (chars.length <= GROUP_NAME_MAX) return cleaned;
  return `${chars.slice(0, GROUP_NAME_MAX - 1).join("")}\u2026`;
}

export function groupRecipient(id: string): string {
  return `group.${id}`;
}

// recipientsToText renders the recipients config value (an array from the server, text while typing).
export function recipientsToText(value: unknown): string {
  if (Array.isArray(value)) return value.map(String).join(", ");
  if (value === null || value === undefined) return "";
  return String(value);
}

export type GroupSelection = { kind: "empty" } | { kind: "selected"; recipient: string } | { kind: "manual" };

// groupSelection decides what the group picker shows for the current recipients value. A value that is
// not in the list (or a list that is missing) stays in manual entry, untouched.
export function groupSelection(groups: GroupOption[] | null, value: unknown): GroupSelection {
  if (!groups || groups.length === 0) return { kind: "manual" };
  const text = recipientsToText(value).trim();
  if (text === "") return { kind: "empty" };
  const hit = groups.find((g) => groupRecipient(g.id) === text);
  return hit ? { kind: "selected", recipient: text } : { kind: "manual" };
}

// relativeAge words how long ago a time was, for "updated 2 min ago".
export function relativeAge(iso: string | null, nowMs: number): string {
  if (!iso) return "never";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "never";
  const s = Math.max(0, Math.floor((nowMs - t) / 1000));
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}
