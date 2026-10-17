// bootstrap and static-element wiring. Dynamically created elements (table rows, the add/edit server form) wire their own listeners
// inside the view module that creates them; this file only wires controls that exist in index.html from page load.

import { USE_LIVE_API, isSignedIn, login, logout, getUsername, fetchServers } from './api.js';
import { configState } from './configstate.js';
import {
  DEFAULT_TAB, TABS, encodeState, decodeState, effectiveWindow,
} from './urlstate.js';
import {
  load, startPolling, stopPolling, refresh,
  setFilter, setFilters, clearFilters, setHideHandshake,
  setUnauthorizedHandler, setUpdatedHandler, setPendingHandler,
  setHoldUpdates, flushPending, getOffsets, setStmtOffset, setConnOffset,
  getFilters, setWindowResolver, connTotal, stmtTotal,
} from './store.js';
import { loadServers, openServerForm } from './views/servers.js';
import { loadAdmins, openAdminForm } from './views/admins.js';

const loginView = document.getElementById("login-view");
const appView = document.getElementById("app-view");
const loginError = document.getElementById("login-error");

function showLogin() {
  loginView.hidden = false; appView.hidden = true;
  const p = document.getElementById("login-pass");
  if (p) p.value = "";
}
function showApp() { loginView.hidden = true; appView.hidden = false; }

// The sign-in screen has three outcomes to tell apart: a wrong password, a
// lockout, and a session that simply ended. Same element, different tone.
function showLoginMessage(kind, text) {
  loginError.className = kind ? "alert " + kind : "alert";
  loginError.textContent = text;
}

// The header shows who is signed in, with initials standing in for an avatar.
function renderWhoami() {
  const name = getUsername();
  const box = document.getElementById("whoami");
  if (!box) return;
  if (!name) { box.hidden = true; return; }
  document.getElementById("whoami-name").textContent = name;
  document.getElementById("whoami-avatar").textContent = name.slice(0, 2).toUpperCase();
  box.hidden = false;
}

// The pill answers "is the proxy running what I saved" from any tab, so it
// polls on its own. Slower than the Servers table, which also needs the rows.
const PILL_POLL_MS = 10000;
let pillTimer = null;
const pill = document.getElementById("proxy-pill");

async function loadProxyPill() {
  if (!USE_LIVE_API || !pill) return;
  try {
    const resp = await fetchServers();
    if (!resp.ok) return;
    const data = await resp.json();
    const state = configState({
      revision: data.revision,
      appliedRevision: data.applied_revision,
      appliedAt: data.applied_at,
    });
    if (!state) { pill.hidden = true; return; }
    pill.className = "proxy-pill " + state.state;
    document.getElementById("proxy-pill-text").textContent = state.text;
    pill.hidden = false;
  } catch (e) {
    // A failed poll leaves the last known state up rather than flickering.
  }
}

function startPillPolling() {
  loadProxyPill();
  if (!pillTimer) pillTimer = setInterval(loadProxyPill, PILL_POLL_MS);
}
function stopPillPolling() { clearInterval(pillTimer); pillTimer = null; }

if (pill) {
  pill.addEventListener("click", (e) => {
    e.preventDefault();
    document.querySelector('.mainnav button[data-view="servers"]')?.click();
  });
}

// "/" jumps to the SQL search unless the operator is already typing somewhere.
document.addEventListener("keydown", (e) => {
  if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
  const t = e.target;
  if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT")) return;
  const q = document.getElementById("filter-q");
  if (q && !q.closest("[hidden]")) { e.preventDefault(); q.focus(); }
});

setUnauthorizedHandler(() => {
  // store.js already clears the session and stops the audit poll before calling
  // this. The pill poller belongs to this file, so it has to be stopped here
  // or it keeps requesting with a dead session.
  stopServersPolling();
  stopPillPolling();
  showLogin();
  showLoginMessage("info", "Your session ended, so you have been signed out. Sign in again to pick up where you left off.");
});

// --- the Activity view ---
// `view` is what the URL describes and what the chips draw from; the store
// gets the resolved version, where a preset range has become two timestamps.

let view = decodeState(location.search);

const RANGE_LABEL = {
  "1h": "Last hour",
  "24h": "Last 24 hours",
  "7d": "Last 7 days",
  "30d": "Last 30 days",
  "custom": "Custom range",
};

// datetime-local yields "2026-10-05T10:28" with no seconds or zone, which the
// API's RFC3339 parse rejects. Send the browser's own offset so the range
// means what the operator sees on screen.
//
// The inputs have minute precision, so an end of 10:28 means up to the end of
// 10:28, not the first instant of it; endOfMinute covers that.
function toRFC3339(localValue, endOfMinute = false) {
  if (!localValue) return "";
  const d = new Date(localValue);          // parsed as local time
  if (isNaN(d)) return "";
  if (endOfMinute) d.setSeconds(59, 999);
  return d.toISOString();                  // UTC, with Z
}

// Rewritten, never pushed: every keystroke in the search would otherwise be a
// history entry for the back button to walk through.
let lastUrl = null;
function writeUrl() {
  const url = location.pathname + encodeState(view);
  if (url === lastUrl) return;
  lastUrl = url;
  try { history.replaceState(null, "", url); } catch (e) {}
}

// Asked again for every request, so a "last hour" preset keeps rolling forward.
setWindowResolver(() => effectiveWindow(view));

function applyView() {
  const win = effectiveWindow(view);
  setFilters({
    user: view.filters.user,
    upstream: view.filters.upstream,
    q: view.filters.q,
    type: view.filters.type,
    conn_id: view.filters.conn_id,
    from: win.from,
    to: win.to,
    outcome: view.failedOnly ? "fail" : "",
  });
  writeUrl();
  renderChips();
}

function syncControls() {
  document.getElementById("filter-q").value = view.filters.q;
  document.getElementById("filter-user").value = view.filters.user;
  document.getElementById("filter-upstream").value = view.filters.upstream;
  document.getElementById("filter-type").value = view.filters.type;
  document.getElementById("hide-handshake").checked = view.hideHandshake;
  document.getElementById("failed-only").checked = view.failedOnly;

  for (const b of document.querySelectorAll("#range-group button")) {
    b.setAttribute("aria-pressed", String(b.dataset.range === view.range));
  }
  document.getElementById("custom-range").hidden = view.range !== "custom";
}

function debounce(fn, ms = 300) {
  let t = null;
  return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
}

function bindText(id, name, transform) {
  const el = document.getElementById(id);
  if (!el) return;
  el.addEventListener("input", debounce(() => {
    const v = el.value.trim();
    view.filters[name] = transform ? transform(v) : v;
    applyView();
  }));
}

bindText("filter-q", "q");
bindText("filter-user", "user");
bindText("filter-upstream", "upstream");
// stmt_type is the first keyword of the statement, uppercased, and the API matches it exactly
bindText("filter-type", "type", (v) => v.toUpperCase());

for (const id of ["filter-from", "filter-to"]) {
  const el = document.getElementById(id);
  if (!el) continue;
  el.addEventListener("change", () => {
    view.range = "custom";
    view.filters.from = toRFC3339(document.getElementById("filter-from").value);
    view.filters.to = toRFC3339(document.getElementById("filter-to").value, true);
    syncControls();
    applyView();
  });
}

document.getElementById("range-group").addEventListener("click", (e) => {
  const btn = e.target.closest("button[data-range]");
  if (!btn) return;
  const picked = btn.dataset.range;
  // Clicking the chosen one again clears it: the only way back to all time.
  view.range = view.range === picked ? null : picked;
  if (view.range !== "custom") { view.filters.from = ""; view.filters.to = ""; }
  syncControls();
  applyView();
});

document.getElementById("hide-handshake").addEventListener("change", (e) => {
  view.hideHandshake = e.target.checked;
  setHideHandshake(view.hideHandshake);
  writeUrl();
  renderChips();
});

document.getElementById("failed-only").addEventListener("change", (e) => {
  view.failedOnly = e.target.checked;
  applyView();
});

// --- chips ---

const chipsEl = document.getElementById("filter-chips");

function addChip(label, onRemove, cls) {
  const chip = document.createElement("span");
  chip.className = cls ? "chip " + cls : "chip";
  chip.appendChild(document.createTextNode(label));
  const x = document.createElement("button");
  x.type = "button";
  x.setAttribute("aria-label", "Remove filter: " + label);
  x.textContent = "×";
  x.addEventListener("click", onRemove);
  chip.appendChild(x);
  chipsEl.appendChild(chip);
}

function clearFilterNamed(name) {
  view.filters[name] = "";
  syncControls();
  applyView();
}

function renderChips() {
  if (!chipsEl) return;
  chipsEl.innerHTML = "";

  const parts = [];
  if (view.filters.user) parts.push(["User: " + view.filters.user, () => clearFilterNamed("user")]);
  if (view.filters.upstream) parts.push(["Server: " + view.filters.upstream, () => clearFilterNamed("upstream")]);
  if (view.filters.type) parts.push(["Type: " + view.filters.type, () => clearFilterNamed("type")]);
  if (view.filters.q) parts.push(['SQL contains: "' + view.filters.q + '"', () => clearFilterNamed("q")]);
  if (view.filters.conn_id) {
    parts.push(["Session: " + view.filters.conn_id.slice(0, 8), () => clearFilterNamed("conn_id")]);
  }
  if (view.range) {
    parts.push([RANGE_LABEL[view.range] || view.range, () => {
      view.range = null;
      view.filters.from = "";
      view.filters.to = "";
      syncControls();
      applyView();
    }]);
  }

  // Failed logins filter sessions only; on Statements the chip would claim a
  // filter that is not applied.
  const showFailed = view.failedOnly && view.tab === "sessions";

  if (parts.length === 0 && !showFailed && view.hideHandshake) return;

  const label = document.createElement("span");
  label.className = "chips-label";
  label.textContent = "Showing";
  chipsEl.appendChild(label);

  for (const [text, remove] of parts) addChip(text, remove);

  if (showFailed) {
    addChip("Failed logins only", () => {
      view.failedOnly = false;
      syncControls();
      applyView();
    }, "warn");
  }
  if (!view.hideHandshake) {
    addChip("Including client handshake", () => {
      view.hideHandshake = true;
      setHideHandshake(true);
      syncControls();
      writeUrl();
      renderChips();
    });
  }

  const clearBtn = document.createElement("button");
  clearBtn.type = "button";
  clearBtn.className = "clear-all";
  clearBtn.textContent = "Clear all";
  clearBtn.addEventListener("click", () => {
    const tab = view.tab;
    view = decodeState("");
    view.tab = tab;
    syncControls();
    setHideHandshake(true);
    clearFilters();
    writeUrl();
    renderChips();
  });
  chipsEl.appendChild(clearBtn);
}

// --- tabs ---

// Both tables are fetched on every poll whichever tab is open, because the tab
// labels carry live counts for both, so switching is a show/hide not a refetch.
function showTab(tab) {
  view.tab = TABS.includes(tab) ? tab : DEFAULT_TAB;
  const isStmt = view.tab === "statements";

  document.getElementById("pane-statements").hidden = !isStmt;
  document.getElementById("pane-sessions").hidden = isStmt;
  for (const b of document.querySelectorAll(".tab")) {
    const on = b.dataset.tab === view.tab;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  }

  document.getElementById("wrap-q").hidden = !isStmt;
  document.getElementById("wrap-type").hidden = !isStmt;
  document.getElementById("opt-handshake").hidden = !isStmt;
  document.getElementById("wrap-upstream").hidden = isStmt;
  document.getElementById("opt-failed").hidden = isStmt;

  renderPending();
  writeUrl();
}

document.querySelector(".tabs").addEventListener("click", (e) => {
  const btn = e.target.closest(".tab");
  if (btn) showTab(btn.dataset.tab);
});

function renderTabCounts() {
  document.getElementById("tabcount-statements").textContent = stmtTotal ? String(stmtTotal) : "";
  document.getElementById("tabcount-sessions").textContent = connTotal ? String(connTotal) : "";
}

// A connection row sets conn_id inside the store, so read it back to keep the URL honest.
setUpdatedHandler(() => {
  const connId = getFilters().conn_id;
  if (connId !== view.filters.conn_id) {
    view.filters.conn_id = connId;
    // The filter applies to statements. Picked from the Sessions tab, nothing
    // on screen would change, so go to the statements it narrowed.
    if (connId && view.tab !== "statements") showTab("statements");
    writeUrl();
    renderChips();
  }
  renderTabCounts();
});

// --- copy link ---

const copyBtn = document.getElementById("copy-link");
copyBtn.addEventListener("click", async () => {
  const original = copyBtn.textContent;
  try {
    await navigator.clipboard.writeText(location.href);
    copyBtn.textContent = "Copied";
  } catch (e) {
    copyBtn.textContent = "Copy from the address bar";
  }
  setTimeout(() => { copyBtn.textContent = original; }, 2000);
});

// --- live/paused toggle ---
// Pausing holds the rendering, not the polling: without the requests there is
// nothing to count, and the banner could never say what arrived.
let paused = false;
const liveToggle = document.getElementById("live-toggle");
liveToggle.addEventListener("click", () => {
  paused = !paused;
  liveToggle.textContent = paused ? "❚❚ Paused" : "● Live";
  liveToggle.classList.toggle("on", !paused);
  setHoldUpdates(paused);
});

// --- rows waiting to be shown ---

const newRows = document.getElementById("new-rows");
const newRowsText = document.getElementById("new-rows-text");
let lastPending = { statements: 0, sessions: 0 };

function renderPending() {
  if (!newRows) return;
  const n = view.tab === "statements" ? lastPending.statements : lastPending.sessions;
  if (!n) { newRows.hidden = true; return; }
  const noun = view.tab === "statements"
    ? (n === 1 ? "statement" : "statements")
    : (n === 1 ? "session" : "sessions");
  newRowsText.textContent = n + " new " + noun;
  newRows.hidden = false;
}

setPendingHandler((counts) => { lastPending = counts; renderPending(); });

document.getElementById("new-rows-show").addEventListener("click", () => {
  // Paged away from the newest rows, going back is what "show them" means.
  const off = getOffsets();
  if (view.tab === "statements" && off.stmt > 0) setStmtOffset(0);
  else if (view.tab === "sessions" && off.conn > 0) setConnOffset(0);
  else flushPending();
});

// A hidden tab does not need five requests a minute. Coming back asks once,
// which the hold rules may still turn into a banner rather than a redraw.
document.addEventListener("visibilitychange", () => {
  if (views.audit.hidden) return;
  if (document.hidden) { stopPolling(); return; }
  startPolling();
  refresh();
});

// --- servers screen ---
document.getElementById("add-server-btn").addEventListener("click", () => openServerForm(null));
//--- admin screen ---
document.getElementById("add-admin-btn").addEventListener("click", () => openAdminForm());

// The status line claims whether saved changes are live, so it has to keep
// refreshing while the screen is open. Slower than the audit log: config
// changes are rare.
const SERVERS_POLL_MS = 5000;
let serversTimer = null;
function startServersPolling() {
  if (!serversTimer) serversTimer = setInterval(loadServers, SERVERS_POLL_MS);
}
function stopServersPolling() { clearInterval(serversTimer); serversTimer = null; }

// --- nav ---
const views = {
  audit: document.getElementById("view-audit"),
  servers: document.getElementById("view-servers"),
  admins: document.getElementById("view-admins"),
};
document.querySelectorAll(".mainnav button").forEach((btn) => {
  btn.addEventListener("click", () => {
    const target = btn.dataset.view;
    for (const name in views) views[name].hidden = (name !== target);
    document.querySelectorAll(".mainnav button").forEach((b) => b.classList.toggle("active", b === btn));
    if (target === "servers") {
      stopPolling();
      loadServers();
      startServersPolling();
    } else if (target === "admins") {
      stopPolling();
      stopServersPolling();
      loadAdmins();
    } else {
      stopServersPolling();
      startPolling();
      refresh(true);
    }
    // Remember the tab, so a reload doesn't send the operator back to the audit log.
    try { localStorage.setItem("view", target); } catch (e) {}
  });
});

// Restoring by clicking reuses the handler above, so polling and the active
// class stay correct rather than being duplicated here.
function restoreView() {
  // The Servers and Admins tabs are hidden in mock mode, so don’t restore to them
  if (!USE_LIVE_API) return;
  try {
    const saved = localStorage.getItem("view");
    if (saved && saved !== "audit" && views[saved]) {
      document.querySelector(`.mainnav button[data-view="${saved}"]`)?.click();
    }
  } catch (e) {}
}

// Servers config and admin accounts are live-only (no mock data); hide the tabs in mock mode.
if (!USE_LIVE_API) {
  const ns = document.getElementById("nav-servers");
  if (ns) ns.hidden = true;
  const na = document.getElementById("nav-admins");
  if (na) na.hidden = true;
}

// The mock files are fixed, so server-side filtering has nothing to act on.
// That includes the handshake toggle: the API applies it, so in mock mode the
// checkbox is disabled rather than left as a control that does nothing.
if (!USE_LIVE_API) {
  const bar = document.getElementById("filter-bar");
  if (bar) bar.hidden = true;
  const hs = document.getElementById("hide-handshake");
  if (hs) {
    hs.checked = false;
    hs.disabled = true;
    hs.title = "Applied by the API: not available in mock mode.";
  }
}

// --- login / logout ---
document.getElementById("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  showLoginMessage("", "");
  const username = document.getElementById("login-user").value;
  const password = document.getElementById("login-pass").value;
  document.getElementById("login-pass").value = "";
  try {
    await login(username, password);
  } catch (err) {
    // Four outcomes, each needing something different from the operator. 429
    // is the login rate limit from #80, which is per address as well as per
    // username and is not lifted by resetting a password: waiting is the only
    // fix. No status at all means the request never got an answer, and 5xx is
    // the edge failing to reach the API; neither says anything about the
    // password.
    if (err.status === 429) {
      showLoginMessage("warn", "Too many failed sign-in attempts. Wait up to 15 minutes and try again.");
    } else if (!err.status || err.status >= 500) {
      showLoginMessage("error", "Could not reach the server. Check that it is running and try again.");
    } else if (err.status === 401) {
      showLoginMessage("error", "That username and password do not match. Check them and try again.");
    } else {
      showLoginMessage("error", "Sign-in failed (" + err.status + "). Try again.");
    }
    return;
  }
  showApp(); renderWhoami(); startPillPolling();
  startAudit(); restoreView();
});

document.getElementById("logout-btn").addEventListener("click", async () => {
  // Back to the audit log before signing out, so the next sign-in starts there
  // rather than on whichever tab this session left open.
  document.querySelector('.mainnav button[data-view="audit"]')?.click();
  stopPolling(); stopServersPolling(); stopPillPolling();
  showLoginMessage("", "");
  showLogin();
  await logout();
});

function startAudit() {
  syncControls();
  showTab(view.tab);
  setHideHandshake(view.hideHandshake);
  applyView();
  load();
  startPolling();
}

if (!USE_LIVE_API) { showApp(); startAudit(); restoreView(); }
else if (isSignedIn()) { showApp(); renderWhoami(); startPillPolling(); startAudit(); restoreView(); }
else { showLogin(); }