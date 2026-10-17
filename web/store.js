// audit data state, polling loop, and the connection/handshake
// filter. Shares the unauthorized-handling sequence with views/servers.js
// via handleUnauthorized(), so both stay in lockstep without store.js
// depending on main.js directly.
//
// Filtering is server-side (#57): these arrays hold the current page, not
// the whole log.

import { clearSession, fetchConnections, fetchStatements } from './api.js';
import { renderConnections, highlightSelected, renderSelectedConn } from './views/connections.js';
import { renderStatements } from './views/statements.js';

export let allConnections = [];
export let allStatements = [];
export let connTotal = 0;
export let stmtTotal = 0;

export const PAGE_SIZE = 50; // the API's own default; its ceiling is 500

// Empty strings are omitted from the query string by buildQuery.
const filters = {
  user: "",
  upstream: "",   // connections only
  type: "",       // statements only
  q: "",          // statements only (the API's only LIKE match)
  from: "",
  to: "",
  conn_id: "",    // statements only; exact match, set by clicking a row
  outcome: "",    // connections only; "fail" for the failed-logins toggle
};

let connOffset = 0;
let stmtOffset = 0;
let refreshSeq = 0;

// Where the time window comes from at request time. A preset such as "last
// hour" is relative to now, so resolving it once when it is chosen would freeze
// it; the caller supplies a function that is asked again for every poll. It
// returns { from, to }, and without one the stored filters are used as they are.
let windowResolver = null;
export function setWindowResolver(fn) { windowResolver = fn; }

export function getFilters() { return { ...filters }; }
export function getOffsets() { return { conn: connOffset, stmt: stmtOffset }; }

// Changing a filter resets both offsets: page 4 of a result set that no
// longer has four pages would show an empty table for no visible reason.
export function setFilter(name, value) {
  if (!(name in filters)) return;
  filters[name] = value;
  connOffset = 0;
  stmtOffset = 0;
  refresh(true);
}

// Several filters at once, for restoring a whole view from the URL. Doing it
// through setFilter would fire one request per filter and leave the earlier
// ones racing the later ones.
export function setFilters(values) {
  for (const [name, value] of Object.entries(values || {})) {
    if (name in filters) filters[name] = value ?? "";
  }
  connOffset = 0;
  stmtOffset = 0;
  refresh(true);
}

export function clearFilters() {
  for (const k of Object.keys(filters)) filters[k] = "";
  connOffset = 0;
  stmtOffset = 0;
  refresh(true);
}

export function hasActiveFilter() {
  return Object.values(filters).some((v) => v !== "");
}

export function setConnOffset(n) { connOffset = Math.max(0, n); refresh(true); }
export function setStmtOffset(n) { stmtOffset = Math.max(0, n); refresh(true); }

export function selectConnection(connId) {
  filters.conn_id = connId;
  stmtOffset = 0;
  refresh(true);
}

// Statements every MySQL client sends at connection startup are hidden by the
// API (hide_handshake), before it paginates, so a page is always full of real
// statements and the total reflects the filter. The list of what counts as
// handshake lives in internal/store/store.go. Changing the toggle is a new
// query, so it goes back to the first page and refetches.
let hideHandshake = true; // toggle state (default)
export function setHideHandshake(v) {
  hideHandshake = v;
  stmtOffset = 0;
  refresh(true);
}
// Read back so the URL can carry it, since it is state the view depends on
// but does not live in filters.
export function getHideHandshake() { return hideHandshake; }

// --- holding updates back ---
//
// Polling never stops while the screen is open, but the rendering does. A
// table that reorders itself under the cursor is unreadable, so new rows wait
// when the operator has paused, or has paged away from the newest rows, and a
// banner offers them instead. An explicit action (a filter, a page, a tab)
// always renders at once: that is the operator asking for new output.

let holdUpdates = false;
let pending = null;        // the newest poll result, not yet shown
let shownConnTotal = 0;    // the totals the visible tables were drawn from
let shownStmtTotal = 0;

let onPending = null;
export function setPendingHandler(fn) { onPending = fn; }

function heldBack() { return holdUpdates || stmtOffset > 0 || connOffset > 0; }

export function pendingCounts() {
  if (!pending) return { statements: 0, sessions: 0 };
  return {
    statements: Math.max(0, pending.stmtTotal - shownStmtTotal),
    sessions: Math.max(0, pending.connTotal - shownConnTotal),
  };
}

function announcePending() {
  if (onPending) onPending(pendingCounts());
}

export function setHoldUpdates(v) {
  holdUpdates = v;
  if (v) return;
  // Resuming shows whatever arrived while paused, or fetches if nothing did.
  if (pending) flushPending(); else refresh(true);
}
export function getHoldUpdates() { return holdUpdates; }

export function flushPending() {
  if (!pending) return;
  allConnections = pending.connections;
  allStatements = pending.statements;
  connTotal = pending.connTotal;
  stmtTotal = pending.stmtTotal;
  pending = null;
  shownConnTotal = connTotal;
  shownStmtTotal = stmtTotal;
  renderCurrent();
  announcePending();
  if (onUpdated) onUpdated();
}

// Re-renders what is already loaded, for display-only changes.
function renderCurrent() {
  renderStatements(allStatements, 0, stmtTotal, stmtOffset, allStatements.length, setStmtOffset, PAGE_SIZE);
  renderConnections(allConnections, connTotal, connOffset);
  highlightSelected(filters.conn_id);
  renderSelectedConn(filters.conn_id);
}

// --- 401 handling, shared by refresh() and loadServers() ---
let onUnauthorized = null;
export function setUnauthorizedHandler(fn) { onUnauthorized = fn; }
export function handleUnauthorized() {
  clearSession();
  stopPolling();
  if (onUnauthorized) onUnauthorized();
}

// Called after every successful refresh, so the tab counts and the filter
// chips can follow the data without polling on their own schedule.
let onUpdated = null;
export function setUpdatedHandler(fn) { onUpdated = fn; }

// --- audit data polling ---
// force: this refresh is the result of the operator asking for something, so
// it renders even when updates are otherwise held back.
export async function refresh(force = false) {
  const seq = ++refreshSeq;

  const win = windowResolver ? windowResolver() : { from: filters.from, to: filters.to };

  const connQuery = {
    user: filters.user,
    upstream: filters.upstream,
    outcome: filters.outcome,
    from: win.from,
    to: win.to,
    limit: PAGE_SIZE,
    offset: connOffset,
  };

  const stmtQuery = {
    conn_id: filters.conn_id,
    user: filters.user,
    type: filters.type,
    q: filters.q,
    from: win.from,
    to: win.to,
    limit: PAGE_SIZE,
    offset: stmtOffset,
  };

  if (hideHandshake) {
    stmtQuery.hide_handshake = "1";
  }

  try {
    const [connResp, stmtResp] = await Promise.all([
      fetchConnections(connQuery),
      fetchStatements(stmtQuery),
    ]);
    if (seq !== refreshSeq) return; // a newer refresh overtook this one
    if (connResp.status === 401 || stmtResp.status === 401) { handleUnauthorized(); return; }

    const connData = await connResp.json();
    const stmtData = await stmtResp.json();
    if (seq !== refreshSeq) return; // and again: parsing the bodies is async too

    const connections = connData.connections || [];
    const statements = stmtData.statements || [];
    // Mock files carry no total; fall back to the page length.
    const nextConnTotal = connData.total ?? connections.length;
    const nextStmtTotal = stmtData.total ?? statements.length;

    if (!force && heldBack()) {
      pending = { connections, statements, connTotal: nextConnTotal, stmtTotal: nextStmtTotal };
      announcePending();
      markUpdated();
      return;
    }

    allConnections = connections;
    allStatements = statements;
    connTotal = nextConnTotal;
    stmtTotal = nextStmtTotal;
    pending = null;
    shownConnTotal = connTotal;
    shownStmtTotal = stmtTotal;

    renderCurrent();
    announcePending();
    markUpdated();
    if (onUpdated) onUpdated();
  } catch (e) { markError(); }
}
export async function load() { await refresh(true); }

export const POLL_MS = 5000;
let pollTimer = null;
// Wrapped rather than passed directly, so a future argument to refresh cannot
// arrive from the timer.
export function startPolling() { if (!pollTimer) pollTimer = setInterval(() => refresh(), POLL_MS); }
export function stopPolling() { clearInterval(pollTimer); pollTimer = null; }

function markUpdated() {
  const el = document.getElementById("updated");
  if (el) el.textContent = "updated " + new Date().toLocaleTimeString();
}
function markError() {
  const el = document.getElementById("updated");
  if (el) el.textContent = "update failed — retrying…";
}