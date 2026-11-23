import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// No React renderer is available to node --test, so this checks the source of the card: the
// Norma-controlled components are rendered as disabled, unchecked checkboxes with the note, and
// the configurable ones are never listed among them.
const src = readFileSync(new URL("../app/(main)/system/settings/user-agent-card.tsx", import.meta.url), "utf8");

test("WebFetch and WebSearch are locked, disabled and explained", () => {
  assert.match(src, /LOCKED_COMPONENTS = \["WebFetch", "WebSearch"\] as const/);
  const locked = src.slice(src.indexOf("LOCKED_COMPONENTS.map"));
  assert.match(locked, /<Checkbox id=\{`ua-c-\$\{name\}`\} className="mt-0\.5" checked=\{false\} disabled \/>/);
  assert.match(locked, /controlled by the Norma library; not configurable yet/);
});

test("the master switch label is present", () => {
  assert.match(src, /Hide ARTEX user agent/);
});
