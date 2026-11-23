import {
  displayedIsolation,
  ISOLATION_POLL_MS,
  ISOLATION_UNKNOWN_AFTER,
  isolationBannerKind,
  isolationBannerText,
  nextIsolationState,
} from "./isolation-banner.ts";
import assert from "node:assert/strict";
import test from "node:test";

const t = (status, warnings = []) => ({ status, reason: "", warnings });

test("enforced without warnings and inactive show nothing, a missing status shows nothing yet", () => {
  assert.equal(isolationBannerKind(t("enforced")), "none");
  assert.equal(isolationBannerKind(t("inactive")), "none");
  assert.equal(isolationBannerKind(undefined), "none");
  assert.equal(isolationBannerKind(null), "none");
});

test("a status string the page does not know is the unknown notice, never none", () => {
  assert.equal(isolationBannerKind(t("something-new")), "unknown");
  assert.equal(isolationBannerKind(t("")), "unknown");
  assert.equal(isolationBannerKind(t("unknown")), "unknown");
  assert.equal(isolationBannerKind({ reason: "", warnings: [] }), "unknown");
});

test("unavailable and disabled are the non-dismissible kind", () => {
  assert.equal(isolationBannerKind(t("unavailable")), "unavailable");
  assert.equal(isolationBannerKind(t("disabled")), "unavailable");
});

test("degraded is collapsible, warnings alone are a notice", () => {
  assert.equal(isolationBannerKind(t("degraded")), "degraded");
  assert.equal(isolationBannerKind(t("enforced", ["x"])), "notice");
});

test("texts are neutral: no reason, no paths", () => {
  for (const s of ["unavailable", "disabled", "degraded", "unknown", "enforced"]) {
    const x = { status: s, reason: "C3: /run/secret path", warnings: ["w /run/x"] };
    const { title, body } = isolationBannerText(x);
    assert.ok(title && body);
    assert.ok(!`${title}${body}`.includes("/run"));
    assert.ok(!/attack|exploit|steal/i.test(`${title}${body}`));
  }
});

test("the unknown notice has the agreed title", () => {
  assert.equal(isolationBannerText(t("unknown")).title, "Tool isolation status is unknown");
});

test("the status is refreshed every 60 seconds and goes unknown after two failures in a row", () => {
  assert.equal(ISOLATION_POLL_MS, 60_000);
  assert.equal(ISOLATION_UNKNOWN_AFTER, 2);
});

test("a failed refresh keeps the last known status; two in a row show the unknown notice", () => {
  let s = nextIsolationState({ isolation: null, failures: 0 }, { ok: true, isolation: t("degraded") });
  assert.equal(displayedIsolation(s).status, "degraded");
  s = nextIsolationState(s, { ok: false });
  assert.equal(displayedIsolation(s).status, "degraded", "one failure keeps the last status");
  assert.equal(isolationBannerKind(displayedIsolation(s)), "degraded");
  s = nextIsolationState(s, { ok: false });
  assert.equal(isolationBannerKind(displayedIsolation(s)), "unknown", "two failures show the notice");
  s = nextIsolationState(s, { ok: false });
  assert.equal(isolationBannerKind(displayedIsolation(s)), "unknown");
  // A success clears the notice and the count.
  s = nextIsolationState(s, { ok: true, isolation: t("enforced") });
  assert.equal(isolationBannerKind(displayedIsolation(s)), "none");
  assert.equal(s.failures, 0);
  s = nextIsolationState(s, { ok: false });
  assert.equal(isolationBannerKind(displayedIsolation(s)), "none", "the count restarted after the success");
});

test("two failures before any status was known also show the notice, and a success without a status shows nothing", () => {
  let s = { isolation: null, failures: 0 };
  s = nextIsolationState(s, { ok: false });
  assert.equal(isolationBannerKind(displayedIsolation(s)), "none");
  s = nextIsolationState(s, { ok: false });
  assert.equal(isolationBannerKind(displayedIsolation(s)), "unknown");
  s = nextIsolationState(s, { ok: true, isolation: undefined });
  assert.equal(isolationBannerKind(displayedIsolation(s)), "none");
});
