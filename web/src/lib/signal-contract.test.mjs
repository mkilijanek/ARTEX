// Bodies shaped like the ones the server tests build for the Signal routes. They pin the wire format the
// card and the group picker read.
import {
  cleanGroupName,
  groupRecipient,
  groupSelection,
} from "../app/(main)/system/notify/_components/channel-fields.ts";
import { signalCardView } from "./signal-account.ts";
import assert from "node:assert/strict";
import test from "node:test";

const GROUP_ID = "group.QUFFQ0F3UUZCZ2NJQ1FvTERBME9EeEFSRWhNVUZSWVhHQmthR3h3ZEhoOD0=";

const groupsBody = {
  groups: [{ id: GROUP_ID, name: "Ops room", masked: `group.…${GROUP_ID.slice(-4)}` }],
  fetched_at: "2026-11-24T09:58:00Z",
};

const statusBody = {
  mode: "on",
  sidecar: { alive: true, heartbeat_age_s: 2, version: "0.6.1", signal_cli_version: "0.14.10" },
  state: "linked",
  gen: 7,
  account: { number_hint: "+•••0101", confirmed: true },
  linking: null,
  groups_count: 1,
  groups_at: "2026-11-24T09:58:00Z",
  remote_unlink: "",
  last_error: "",
  last_error_at: null,
  db_role: { state: "ok", detail: "" },
  db_auth: "ok",
  warnings: [{ kind: "example", text: "example warning" }, { kind: "bare" }],
};

test("a group id from the server goes unchanged into the recipient and back into the selection", () => {
  const g = groupsBody.groups[0];
  const recipient = groupRecipient(g.id);
  assert.equal(recipient, GROUP_ID);
  assert.deepEqual(groupSelection(groupsBody.groups, [recipient]), { kind: "selected", recipient: GROUP_ID });
  assert.equal(cleanGroupName(g.name), "Ops room");
});

test("the status body of the server maps to a confirmed card with text warnings", () => {
  const v = signalCardView(statusBody, Date.parse("2026-11-24T10:00:00Z"));
  assert.equal(v.state.kind, "linked_confirmed");
  assert.deepEqual(v.warnings, [
    { kind: "other", text: "example warning" },
    { kind: "other", text: "bare" },
  ]);
});
