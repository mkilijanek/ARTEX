// Client-side preview of the User-Agent each component sends. It mirrors useragent.For and
// useragent.Effective in the Go server (useragent/useragent.go); the same fixture is asserted on
// both sides, and the page refreshes from the server after every save.

export const UA_COMPONENTS = ["LLM", "Enrichment", "MCP", "Notify", "Tools", "API Recon"] as const;
export type UAComponent = (typeof UA_COMPONENTS)[number];

export const UA_GO_DEFAULT = "Go default (Go-http-client/1.1 or /2.0)";
export const UA_API_RECON_DEFAULT = "script default (Mozilla/5.0 (spa-api-recon) / Mozilla/5.0 (spa-api-recon spider))";

export const UA_MAX_LENGTH = 256;
export const UA_ALLOWED_SET_MESSAGE =
  "Only printable Latin-1 characters are allowed (U+0020-U+007E and U+00A1-U+00FF, without U+00AD), at most 256";

/**
 * The server's rule (agent.ValidateUserAgent): after trimming, at most 256 characters, each a
 * printable character of the 8-bit set. Returns an error message, or "" when the value is fine.
 */
export function validateUserAgent(raw: string): string {
  const value = raw.trim();
  let count = 0;
  for (const ch of value) {
    count++;
    const code = ch.codePointAt(0) as number;
    const printable = (code >= 0x20 && code <= 0x7e) || (code >= 0xa1 && code <= 0xff && code !== 0xad);
    if (!printable) return UA_ALLOWED_SET_MESSAGE;
  }
  return count > UA_MAX_LENGTH ? UA_ALLOWED_SET_MESSAGE : "";
}

export interface UAInputs {
  hidden: boolean;
  custom: string;
  components: readonly string[];
  version: string;
}

/** The ARTEX string for a component, or the custom value; "" when the component sends no ARTEX User-Agent. */
export function resolveUserAgent(c: UAInputs, component: string): string {
  if (c.hidden || !c.components.includes(component)) return "";
  const custom = c.custom.trim();
  if (custom) return custom;
  return `Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 ARTEX/${c.version} (+${component})`;
}

/** What actually goes out for a component, including the defaults used when nothing is resolved. */
export function effectiveUserAgent(c: UAInputs, component: UAComponent): string {
  const ua = resolveUserAgent(c, component);
  if (ua) return ua;
  return component === "API Recon" ? UA_API_RECON_DEFAULT : UA_GO_DEFAULT;
}
