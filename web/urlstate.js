// The Activity screen's filters and open tab, carried in the query string so a view can be linked to.
export const DEFAULT_TAB = "statements";
export const TABS = ["statements", "sessions"];

// The preset windows, in milliseconds back from now.
export const RANGES = {
  "1h": 60 * 60 * 1000,
  "24h": 24 * 60 * 60 * 1000,
  "7d": 7 * 24 * 60 * 60 * 1000,
  "30d": 30 * 24 * 60 * 60 * 1000,
};

// No preset is selected by default. 
export const DEFAULT_RANGE = null;

// The filters that are plain text and map one to one onto an API parameter.
const TEXT_FILTERS = ["user", "upstream", "q", "type", "conn_id"];

function emptyState() {
  return {
    tab: DEFAULT_TAB,
    range: DEFAULT_RANGE,
    filters: { user: "", upstream: "", q: "", type: "", conn_id: "", from: "", to: "" },
    hideHandshake: true,
    failedOnly: false,
  };
}

// A preset range is stored as the preset
// Only a custom range pins actual times.
export function encodeState(state) {
  const s = Object.assign(emptyState(), state);
  s.filters = Object.assign(emptyState().filters, state && state.filters);
  const q = new URLSearchParams();

  if (s.tab && s.tab !== DEFAULT_TAB && TABS.includes(s.tab)) q.set("tab", s.tab);

  for (const key of TEXT_FILTERS) {
    const v = (s.filters[key] || "").trim();
    if (v) q.set(key, v);
  }

  if (s.range) q.set("range", s.range);
  if (!s.range || s.range === "custom") {
    if (s.filters.from) q.set("from", s.filters.from);
    if (s.filters.to) q.set("to", s.filters.to);
  }

  // Both of these are written only when they differ from the default
  if (!s.hideHandshake) q.set("handshake", "1");
  if (s.failedOnly) q.set("failed", "1");

  const out = q.toString();
  return out ? "?" + out : "";
}

// Anything unrecognised falls back to the default rather than being carried through
export function decodeState(search) {
  const s = emptyState();
  const q = new URLSearchParams(search || "");

  const tab = q.get("tab");
  if (tab && TABS.includes(tab)) s.tab = tab;

  for (const key of TEXT_FILTERS) {
    const v = q.get(key);
    if (v) s.filters[key] = v;
  }

  const range = q.get("range");
  if (range && (range === "custom" || Object.prototype.hasOwnProperty.call(RANGES, range))) {
    s.range = range;
  }

  if (!s.range || s.range === "custom") {
    s.filters.from = q.get("from") || "";
    s.filters.to = q.get("to") || "";
  }

  s.hideHandshake = q.get("handshake") !== "1";
  s.failedOnly = q.get("failed") === "1";

  return s;
}

// Turns a preset into the from/to the API wants. A preset is a rolling window:
// it has a start and no end. An end pinned at "now" would exclude everything
// that arrives afterwards, so a live view of "the last hour" would stop
// showing new rows the moment it was chosen. The start moves with `now`, so
// this is called again for every request rather than once when it is picked.
export function rangeToWindow(range, now = Date.now()) {
  if (!range || range === "custom") return null;
  const span = RANGES[range];
  if (!span) return null;
  return { from: new Date(now - span).toISOString(), to: "" };
}

// The from/to actually sent to the API: the preset's window if one is chosen,
export function effectiveWindow(state, now = Date.now()) {
  const w = rangeToWindow(state && state.range, now);
  if (w) return w;
  const f = (state && state.filters) || {};
  return { from: f.from || "", to: f.to || "" };
}