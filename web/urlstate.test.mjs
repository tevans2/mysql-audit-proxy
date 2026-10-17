import test from "node:test";
import assert from "node:assert/strict";
import {
  encodeState, decodeState, rangeToWindow, effectiveWindow,
  DEFAULT_TAB, RANGES,
} from "./urlstate.js";

const NOW = Date.parse("2026-10-15T12:00:00.000Z");

test("a default view encodes to an empty query string", () => {
  assert.equal(encodeState(decodeState("")), "");
});

test("decoding nothing gives the defaults", () => {
  const s = decodeState("");
  assert.equal(s.tab, DEFAULT_TAB);
  assert.equal(s.range, null);
  assert.equal(s.hideHandshake, true);
  assert.equal(s.failedOnly, false);
  assert.equal(s.filters.q, "");
});

test("the default tab is left out, the other one is written", () => {
  assert.equal(encodeState({ tab: "statements" }), "");
  assert.equal(encodeState({ tab: "sessions" }), "?tab=sessions");
});

test("an unknown tab falls back to the default rather than being carried", () => {
  assert.equal(decodeState("?tab=wibble").tab, DEFAULT_TAB);
});

test("text filters round-trip, and empty ones are left out", () => {
  const s = decodeState("?user=alice&q=DROP+TABLE");
  assert.equal(s.filters.user, "alice");
  assert.equal(s.filters.q, "DROP TABLE");
  assert.equal(s.filters.upstream, "");
  const again = decodeState(encodeState(s));
  assert.deepEqual(again.filters, s.filters);
});

test("whitespace-only filters do not reach the URL", () => {
  assert.equal(encodeState({ filters: { user: "   " } }), "");
});

test("values needing escaping survive the round trip", () => {
  const sql = "SELECT * FROM t WHERE a = 'b&c' -- #1";
  const s = decodeState(encodeState({ filters: { q: sql } }));
  assert.equal(s.filters.q, sql);
});

test("a preset range is stored as the preset, not as two timestamps", () => {
  const out = encodeState({ range: "24h", filters: { from: "x", to: "y" } });
  assert.equal(out, "?range=24h");
});

test("a custom range carries its own from and to", () => {
  const out = encodeState({
    range: "custom",
    filters: { from: "2026-10-01T00:00:00.000Z", to: "2026-10-02T00:00:00.000Z" },
  });
  assert.match(out, /range=custom/);
  assert.match(out, /from=2026-10-01/);
  assert.match(out, /to=2026-10-02/);
});

test("from and to survive with no range at all", () => {
  const s = decodeState("?from=2026-10-01T00%3A00%3A00.000Z");
  assert.equal(s.range, null);
  assert.equal(s.filters.from, "2026-10-01T00:00:00.000Z");
});

test("an unknown range is dropped", () => {
  assert.equal(decodeState("?range=since-tuesday").range, null);
});

test("the two booleans are written only when they differ from the default", () => {
  assert.equal(encodeState({ hideHandshake: true, failedOnly: false }), "");
  assert.equal(encodeState({ hideHandshake: false }), "?handshake=1");
  assert.equal(encodeState({ failedOnly: true }), "?failed=1");
  assert.equal(decodeState("?handshake=1").hideHandshake, false);
  assert.equal(decodeState("?failed=1").failedOnly, true);
});

test("rangeToWindow turns a preset into a window that starts a span ago", () => {
  const w = rangeToWindow("1h", NOW);
  assert.equal(w.from, "2026-10-15T11:00:00.000Z");
  assert.equal(Date.parse("2026-10-15T12:00:00.000Z") - Date.parse(w.from), RANGES["1h"]);
});

// A pinned end would hide every row that arrives after the preset is chosen,
// which is the whole point of a live view.
test("a preset window has no end, so rows that arrive later are still inside it", () => {
  for (const range of Object.keys(RANGES)) {
    assert.equal(rangeToWindow(range, NOW).to, "", range);
  }
});

test("a preset's start moves forward each time it is resolved", () => {
  const first = effectiveWindow({ range: "1h" }, NOW);
  const later = effectiveWindow({ range: "1h" }, NOW + 30 * 60 * 1000);
  assert.equal(Date.parse(later.from) - Date.parse(first.from), 30 * 60 * 1000);
  assert.equal(later.to, "");
});

test("rangeToWindow declines to invent one for custom or for nothing", () => {
  assert.equal(rangeToWindow("custom", NOW), null);
  assert.equal(rangeToWindow(null, NOW), null);
  assert.equal(rangeToWindow("since-tuesday", NOW), null);
});

test("effectiveWindow prefers the preset and falls back to the custom inputs", () => {
  assert.equal(effectiveWindow({ range: "24h" }, NOW).from, "2026-10-14T12:00:00.000Z");
  const custom = effectiveWindow(
    { range: "custom", filters: { from: "A", to: "B" } }, NOW,
  );
  assert.deepEqual(custom, { from: "A", to: "B" });
  assert.deepEqual(effectiveWindow({}, NOW), { from: "", to: "" });
});

test("a full view round-trips unchanged", () => {
  const url = "?tab=sessions&user=alice&upstream=db-a&q=orders&range=7d&failed=1&handshake=1";
  const s = decodeState(url);
  const again = decodeState(encodeState(s));
  assert.deepEqual(again, s);
});