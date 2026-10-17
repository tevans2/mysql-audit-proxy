import test from "node:test";
import assert from "node:assert/strict";
import { configState, REPORT_STALE_MS } from "./configstate.js";

const NOW = Date.parse("2026-10-15T12:00:00.000000Z");
const stamp = (msAgo) => new Date(NOW - msAgo).toISOString();

test("returns null before the API has answered", () => {
  assert.equal(configState({ now: NOW }), null);
  assert.equal(configState({ revision: null, now: NOW }), null);
});

test("unknown when the proxy has never reported", () => {
  const got = configState({ revision: 5, appliedRevision: 0, appliedAt: "", now: NOW });
  assert.equal(got.state, "unknown");
});

test("pending when the saved revision is ahead of the applied one", () => {
  const got = configState({
    revision: 6, appliedRevision: 5, appliedAt: stamp(1000), now: NOW,
  });
  assert.equal(got.state, "pending");
  assert.match(got.text, /rev 6/);
  assert.match(got.text, /rev 5/);
});

test("pending wins over a stale report, because the change is the bigger news", () => {
  const got = configState({
    revision: 6, appliedRevision: 5, appliedAt: stamp(10 * REPORT_STALE_MS), now: NOW,
  });
  assert.equal(got.state, "pending");
});

test("live when the revisions match and the report is recent", () => {
  const got = configState({
    revision: 7, appliedRevision: 7, appliedAt: stamp(5000), now: NOW,
  });
  assert.equal(got.state, "live");
  assert.match(got.text, /rev 7/);
});

test("stale once the report is older than two heartbeats", () => {
  const got = configState({
    revision: 7, appliedRevision: 7, appliedAt: stamp(REPORT_STALE_MS + 1), now: NOW,
  });
  assert.equal(got.state, "stale");
});

test("one missed heartbeat is not yet stale", () => {
  const got = configState({
    revision: 7, appliedRevision: 7, appliedAt: stamp(REPORT_STALE_MS - 1), now: NOW,
  });
  assert.equal(got.state, "live");
});

test("an unparseable timestamp is treated as no report", () => {
  const got = configState({
    revision: 7, appliedRevision: 7, appliedAt: "not a date", now: NOW,
  });
  assert.equal(got.state, "stale");
});
