import { effectiveUserAgent, resolveUserAgent } from "./user-agent.ts";
import assert from "node:assert/strict";
import test from "node:test";

const all = ["LLM", "Enrichment", "MCP", "Notify", "Tools", "API Recon"];

// Same fixture as TestShownFormatFixture in useragent/useragent_test.go.
test("shown format equals the server's", () => {
  const c = { hidden: false, custom: "", components: all, version: "2.2.0" };
  assert.equal(
    resolveUserAgent(c, "API Recon"),
    "Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 ARTEX/2.2.0 (+API Recon)",
  );
});

test("hidden and unchecked components resolve to the defaults", () => {
  const c = { hidden: true, custom: "X/1", components: all, version: "1" };
  assert.equal(resolveUserAgent(c, "LLM"), "");
  assert.equal(effectiveUserAgent(c, "LLM"), "Go default (Go-http-client/1.1 or /2.0)");
  assert.match(effectiveUserAgent(c, "API Recon"), /spa-api-recon/);
  const u = { hidden: false, custom: "X/1", components: ["LLM"], version: "1" };
  assert.equal(effectiveUserAgent(u, "LLM"), "X/1");
  assert.equal(effectiveUserAgent(u, "MCP"), "Go default (Go-http-client/1.1 or /2.0)");
});

test("the custom value is trimmed", () => {
  const c = { hidden: false, custom: "  X/1 ", components: all, version: "1" };
  assert.equal(resolveUserAgent(c, "Tools"), "X/1");
});
