import {
  cleanGroupName,
  formatCountdown,
  groupRecipient,
  groupSelection,
  isLastDigits,
  recipientsToText,
  relativeAge,
} from "../app/(main)/system/notify/_components/channel-fields.ts";
import assert from "node:assert/strict";
import test from "node:test";

const groups = [
  { id: "AAA=", masked: "…AAA=", name: "Security alerts" },
  { id: "BBB=", masked: "…BBB=", name: "Security alerts" },
];

test("formatCountdown prints minutes and seconds and never goes negative", () => {
  assert.equal(formatCountdown(107), "01:47");
  assert.equal(formatCountdown(120), "02:00");
  assert.equal(formatCountdown(0), "00:00");
  assert.equal(formatCountdown(-4), "00:00");
  assert.equal(formatCountdown(Number.NaN), "00:00");
});

test("isLastDigits accepts exactly six ASCII digits", () => {
  assert.equal(isLastDigits("012345"), true);
  assert.equal(isLastDigits("12345"), false);
  assert.equal(isLastDigits("1234567"), false);
  assert.equal(isLastDigits("12345a"), false);
  assert.equal(isLastDigits("١٢٣٤٥٦"), false);
  assert.equal(isLastDigits("123456\n"), false);
});

test("cleanGroupName drops control and format characters, collapses space and cuts", () => {
  assert.equal(cleanGroupName("  Sec\u0000urity\u202E   alerts\n"), "Security alerts");
  assert.equal(cleanGroupName("<img src=x onerror=alert(1)>"), "<img src=x onerror=alert(1)>");
  assert.equal(cleanGroupName("x".repeat(100)).length, 64);
  assert.equal(cleanGroupName("x".repeat(100)).endsWith("…"), true);
  assert.equal(cleanGroupName(""), "");
  assert.equal(cleanGroupName("\u200B\u200B"), "");
});

test("groupRecipient prefixes the id", () => {
  assert.equal(groupRecipient("AAA="), "group.AAA=");
});

test("recipientsToText joins arrays and passes strings", () => {
  assert.equal(recipientsToText(["group.A", "group.B"]), "group.A, group.B");
  assert.equal(recipientsToText("group.A"), "group.A");
  assert.equal(recipientsToText(undefined), "");
  assert.equal(recipientsToText(null), "");
});

test("groupSelection selects a listed group, keeps unknown values manual", () => {
  assert.deepEqual(groupSelection(groups, ["group.BBB="]), { kind: "selected", recipient: "group.BBB=" });
  assert.deepEqual(groupSelection(groups, "group.AAA="), { kind: "selected", recipient: "group.AAA=" });
  assert.deepEqual(groupSelection(groups, ""), { kind: "empty" });
  assert.deepEqual(groupSelection(groups, undefined), { kind: "empty" });
  assert.deepEqual(groupSelection(groups, "group.ZZZ="), { kind: "manual" });
  assert.deepEqual(groupSelection(groups, "group.AAA=, group.BBB="), { kind: "manual" });
  assert.deepEqual(groupSelection(null, ""), { kind: "manual" });
  assert.deepEqual(groupSelection([], "group.AAA="), { kind: "manual" });
});

test("relativeAge words a past time", () => {
  const now = Date.parse("2026-11-24T10:00:00Z");
  assert.equal(relativeAge("2026-11-24T09:59:50Z", now), "just now");
  assert.equal(relativeAge("2026-11-24T09:58:00Z", now), "2 min ago");
  assert.equal(relativeAge("2026-11-24T07:00:00Z", now), "3 h ago");
  assert.equal(relativeAge("2026-11-21T10:00:00Z", now), "3 d ago");
  assert.equal(relativeAge("2026-11-24T10:05:00Z", now), "just now");
  assert.equal(relativeAge(null, now), "never");
  assert.equal(relativeAge("garbage", now), "never");
});
