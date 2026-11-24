import {
  parseSignalError,
  SignalRequestError,
  signalCardView,
  signalFailureKind,
  signalRequest,
} from "./signal-account.ts";
import assert from "node:assert/strict";
import test from "node:test";

const NOW = Date.parse("2026-11-24T10:00:00Z");
const iso = (ms) => new Date(ms).toISOString();
const noop = () => undefined;

const FRESH = { alive: true, heartbeat_age_s: 3, version: "0.6.1", signal_cli_version: "0.14.10" };

const status = (over = {}) => ({
  mode: "on",
  sidecar: FRESH,
  state: "unlinked",
  gen: 1,
  account: { number_hint: "", confirmed: false },
  linking: null,
  groups_count: 0,
  groups_at: null,
  remote_unlink: "",
  last_error: "",
  last_error_at: null,
  db_role: { state: "ok", detail: "" },
  db_auth: "ok",
  warnings: [],
  ...over,
});

const view = (s, phase = "idle") => signalCardView(s, NOW, phase);

test("mode off shows the off view and no warnings", () => {
  assert.deepEqual(view(status({ mode: "off" })), { state: { kind: "off" }, warnings: [] });
});

test("mode on without a fresh heartbeat shows the service as not connected", () => {
  const dead = { ...FRESH, alive: false, heartbeat_age_s: 120 };
  assert.deepEqual(
    view(status({ sidecar: dead, state: "linked", account: { number_hint: "+15… (11 digits)", confirmed: true } })),
    {
      state: { kind: "no_service" },
      warnings: [],
    },
  );
});

test("unknown state with a fresh heartbeat shows the service as starting", () => {
  assert.deepEqual(view(status({ state: "unknown" })), { state: { kind: "unknown" }, warnings: [] });
});

test("unlinked with no removal result and no message shows unlinked", () => {
  assert.deepEqual(view(status()), {
    state: { kind: "unlinked", removedOnSignal: null, lastError: null },
    warnings: [],
  });
});

test("unlinked after an unlink shows whether the device was removed on Signal", () => {
  assert.deepEqual(view(status({ remote_unlink: "local_only" })).state, {
    kind: "unlinked",
    removedOnSignal: "local_only",
    lastError: null,
  });
  assert.deepEqual(view(status({ remote_unlink: "removed" })).state, {
    kind: "unlinked",
    removedOnSignal: "removed",
    lastError: null,
  });
});

test("unlinked after a server-side expiry keeps the message", () => {
  assert.deepEqual(view(status({ last_error: "link expired", last_error_at: iso(NOW - 5000) })).state, {
    kind: "unlinked",
    removedOnSignal: null,
    lastError: "link expired",
  });
});

test("preparing a link shows preparing until the code is fetched", () => {
  assert.deepEqual(view(status(), "preparing"), { state: { kind: "preparing" }, warnings: [] });
});

test("a failed code fetch while preparing shows preparing failed", () => {
  assert.deepEqual(view(status(), "fetch_failed"), { state: { kind: "preparing_failed" }, warnings: [] });
});

test("linking shows the whole seconds left until the expiry", () => {
  const s = status({ state: "linking", linking: { expires_at: iso(NOW + 107_400) } });
  assert.deepEqual(view(s).state, { kind: "linking", secondsLeft: 108 });
});

test("linking at or past the expiry shows expired", () => {
  const s = status({ state: "linking", linking: { expires_at: iso(NOW) } });
  assert.deepEqual(view(s).state, { kind: "expired" });
});

test("linking with a heartbeat older than 30 s shows linking stale and keeps the countdown", () => {
  const old = { ...FRESH, heartbeat_age_s: 31 };
  const s = status({ state: "linking", sidecar: old, linking: { expires_at: iso(NOW + 60_000) } });
  assert.deepEqual(view(s).state, { kind: "linking_stale", secondsLeft: 60 });
});

test("linked and not yet confirmed shows the masked number and the confirmation prompt", () => {
  const s = status({ state: "linked", account: { number_hint: "+15… (11 digits)", confirmed: false } });
  assert.deepEqual(view(s), {
    state: { kind: "linked_unconfirmed", numberHint: "+15… (11 digits)" },
    warnings: [],
  });
});

test("linked and confirmed shows the group count and when it was read", () => {
  const s = status({
    state: "linked",
    account: { number_hint: "+15… (11 digits)", confirmed: true },
    groups_count: 4,
    groups_at: iso(NOW - 120_000),
  });
  assert.deepEqual(view(s).state, {
    kind: "linked_confirmed",
    numberHint: "+15… (11 digits)",
    groupsCount: 4,
    groupsAt: iso(NOW - 120_000),
  });
});

test("error shows the last error with its time and does not count as linked", () => {
  const s = status({ state: "error", last_error: "account no longer linked", last_error_at: iso(NOW - 60_000) });
  assert.deepEqual(view(s).state, {
    kind: "error",
    lastError: "account no longer linked",
    lastErrorAt: iso(NOW - 60_000),
  });
});

test("a database role that is missing or failing is reported as a warning", () => {
  assert.deepEqual(view(status({ db_role: { state: "missing", detail: "" } })).warnings, [{ kind: "db_role_missing" }]);
  assert.deepEqual(view(status({ db_role: { state: "error", detail: "auth failed" } })).warnings, [
    { kind: "db_role_error", detail: "auth failed" },
  ]);
  assert.deepEqual(view(status({ db_role: { state: "unchecked", detail: "" } })).warnings, [
    { kind: "db_role_unchecked" },
  ]);
});

test("database logins that are trusted, unchecked or asserted are reported as warnings", () => {
  assert.deepEqual(view(status({ db_auth: "trust" })).warnings, [{ kind: "db_auth_trust" }]);
  assert.deepEqual(view(status({ db_auth: "unchecked" })).warnings, [{ kind: "db_auth_unchecked" }]);
  assert.deepEqual(view(status({ db_auth: "asserted" })).warnings, [{ kind: "db_auth_asserted" }]);
  assert.deepEqual(view(status({ db_auth: "ok", db_role: { state: "ok", detail: "" } })).warnings, []);
});

test("other warnings from the server are passed through as text", () => {
  assert.deepEqual(view(status({ warnings: [{ kind: "example", text: "example warning" }] })).warnings, [
    { kind: "other", text: "example warning" },
  ]);
});

test("a warning without text shows its kind, and no warning is ever an object", () => {
  const w = view(status({ warnings: [{ kind: "example" }, { kind: "x", text: { a: 1 } }, "plain", null, 4] })).warnings;
  assert.deepEqual(w, [
    { kind: "other", text: "example" },
    { kind: "other", text: "x" },
    { kind: "other", text: "plain" },
  ]);
  for (const x of w) assert.equal(typeof x.text, "string");
});

test("a failed or running link request does not hide the last link error", () => {
  const s = status({ last_error: "link failed" });
  assert.deepEqual(view(s, "preparing").state, { kind: "preparing", lastError: "link failed" });
  assert.deepEqual(view(s, "fetch_failed").state, { kind: "preparing_failed", lastError: "link failed" });
  assert.deepEqual(view(status(), "preparing").state, { kind: "preparing" });
});

test("the failure kinds of the codes the server sends", () => {
  const k = (status, code) => signalFailureKind({ status, code });
  assert.equal(k(409, "unlink_pending"), "unlink_pending");
  assert.equal(k(409, "nothing_to_unlink"), "nothing_to_unlink");
  assert.equal(k(409, "not_linked"), "not_linked");
  assert.equal(k(409, "stale"), "stale");
  assert.equal(k(409, "no_link_pending"), "no_link_pending");
  assert.equal(k(404, "no_link_pending"), "no_link_pending");
  assert.equal(k(503, "db_role_not_ready"), "db_role_not_ready");
  assert.equal(k(503, "busy"), "busy");
  assert.equal(k(503, "sidecar_unavailable"), "unavailable");
});

test("the server text of an error is kept for a 400 or 503 that carries a code", () => {
  const body = JSON.stringify({ error: "Signal state is locked", code: "busy" });
  assert.equal(parseSignalError(503, body, null).serverText, "Signal state is locked");
  assert.equal(parseSignalError(503, "{}", null).serverText, null);
  assert.equal(parseSignalError(503, "not json", null).serverText, null);
});

test("linking without an expiry is rejected as an inconsistent status", () => {
  assert.throws(() => view(status({ state: "linking", linking: null })), /expiry/);
});

test("an unparseable expiry is rejected", () => {
  assert.throws(() => view(status({ state: "linking", linking: { expires_at: "soon" } })), /expiry/);
});

test("an unknown state value is rejected", () => {
  assert.throws(() => view(status({ state: "bogus" })), /state/);
});

test("failure kinds map the password, mismatch, conflict, throttle and unavailable responses", () => {
  assert.equal(signalFailureKind({ status: 403, code: "password_incorrect" }), "password_incorrect");
  assert.equal(signalFailureKind({ status: 403, code: "number_mismatch" }), "number_mismatch");
  assert.equal(signalFailureKind({ status: 409, code: "unlink_first" }), "unlink_first");
  assert.equal(signalFailureKind({ status: 409, code: "link_pending" }), "link_pending");
  assert.equal(signalFailureKind({ status: 409, code: null }), "conflict");
  assert.equal(signalFailureKind({ status: 429, code: null }), "throttled");
  assert.equal(signalFailureKind({ status: 503, code: null }), "unavailable");
  assert.equal(signalFailureKind({ status: 403, code: null }), "failed");
  assert.equal(signalFailureKind({ status: 400, code: null }), "failed");
});

test("an error response keeps the code from the body and the retry delay from the header", () => {
  const wrong = parseSignalError(403, '{"error":"wrong","code":"password_incorrect"}', null);
  assert.ok(wrong instanceof SignalRequestError);
  assert.equal(wrong.status, 403);
  assert.equal(wrong.code, "password_incorrect");
  assert.equal(wrong.retryAfterS, null);

  const limited = parseSignalError(429, '{"error":"rate limited"}', "7");
  assert.equal(limited.code, null);
  assert.equal(limited.retryAfterS, 7);
});

test("a non-JSON error body still reports the status", () => {
  const err = parseSignalError(502, "<html>bad gateway</html>", null);
  assert.equal(err.status, 502);
  assert.equal(err.code, null);
});

function fakeFetch(response) {
  const calls = [];
  const fn = async (url, init) => {
    calls.push({ url, init });
    return response;
  };
  fn.calls = calls;
  return fn;
}

test("a request with a body sends the token and a JSON content type", async () => {
  const f = fakeFetch(new Response('{"gen":2}', { status: 202 }));
  const out = await signalRequest({ fetch: f, token: "tok", onUnauthorized: noop }, "POST", "/notify/signal/link", {
    password: "pw",
    acknowledge: true,
  });
  assert.deepEqual(out, { gen: 2 });
  const { url, init } = f.calls[0];
  assert.equal(url, "/api/notify/signal/link");
  assert.equal(init.method, "POST");
  assert.equal(init.headers.Authorization, "Bearer tok");
  assert.equal(init.headers["Content-Type"], "application/json");
  assert.deepEqual(JSON.parse(init.body), { password: "pw", acknowledge: true });
});

test("a request without a body sends no content type", async () => {
  const f = fakeFetch(new Response('{"mode":"off"}', { status: 200 }));
  await signalRequest({ fetch: f, token: "tok", onUnauthorized: noop }, "GET", "/notify/signal/account");
  const { init } = f.calls[0];
  assert.equal(init.body, undefined);
  assert.equal(init.headers["Content-Type"], undefined);
});

test("a 403 for a wrong password rejects with its code and does not log the user out", async () => {
  let logouts = 0;
  const f = fakeFetch(new Response('{"error":"wrong password","code":"password_incorrect"}', { status: 403 }));
  await assert.rejects(
    signalRequest({ fetch: f, token: "tok", onUnauthorized: () => logouts++ }, "POST", "/notify/signal/unlink", {
      password: "pw",
    }),
    (err) => err instanceof SignalRequestError && err.status === 403 && err.code === "password_incorrect",
  );
  assert.equal(logouts, 0);
});

test("a 401 logs the user out and rejects", async () => {
  let logouts = 0;
  const f = fakeFetch(new Response("", { status: 401 }));
  await assert.rejects(
    signalRequest({ fetch: f, token: "tok", onUnauthorized: () => logouts++ }, "GET", "/notify/signal/account"),
    /Unauthorized/,
  );
  assert.equal(logouts, 1);
});

test("an empty success body resolves to undefined", async () => {
  const f = fakeFetch(new Response("", { status: 202 }));
  const out = await signalRequest({ fetch: f, token: "tok", onUnauthorized: noop }, "DELETE", "/notify/signal/link");
  assert.equal(out, undefined);
});
