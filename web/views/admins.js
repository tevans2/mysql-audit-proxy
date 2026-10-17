// admin account management: list, create, disable, change password, plus the
// attribution log of who changed what. Live-data only (no mock mode).
// Only loadAdmins is called from outside this module.

import { cell } from '../format.js';
import { authFetch, ADMINS_URL, ACTIONS_URL, getUsername } from '../api.js';
import { handleUnauthorized } from '../store.js';
import { actionSentence, matchesGroup } from '../actionlog.js';
import { confirmDialog, formDialog } from './dialog.js';

const adminsBody = document.getElementById("admins-body");
const actionsBody = document.getElementById("actions-body");
const formContainer = document.getElementById("admin-form-container");
const historyFilter = document.getElementById("history-filter");

// GET /api/actions pages, newest first, and defaults to 50. We ask for a
// generous page and filter it in the browser, because the API has no filter
// of its own for admin versus server changes.
const HISTORY_LIMIT = 200;

let lastActions = [];
let historyTotal = 0;
let group = "all";

function tableMessage(tbody, cols, text, isError) {
  tbody.innerHTML = "";
  const tr = document.createElement("tr");
  const td = document.createElement("td");
  td.colSpan = cols;
  td.className = isError ? "table-msg err-text" : "table-msg";
  td.textContent = text;
  tr.appendChild(td);
  tbody.appendChild(tr);
}

//timestamps are whole seconds
function formatStamp(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  return isNaN(d) ? iso : d.toLocaleString();
}

function tag(text, cls) {
  const s = document.createElement("span");
  s.className = "tag " + cls;
  s.textContent = text;
  return s;
}

export async function loadAdmins() {
  try {
    const resp = await authFetch(ADMINS_URL);
    if (resp.status === 401) { handleUnauthorized(); return; }
    if (!resp.ok) { tableMessage(adminsBody, 4, "Failed to load admins (" + resp.status + ")", true); return; }
    const data = await resp.json();
    renderAdmins(data.admins || []);
  } catch (e) {
    tableMessage(adminsBody, 4, "Could not reach the server.", true);
  }
  loadActions();
}

async function loadActions() {
  try {
    const resp = await authFetch(ACTIONS_URL + "?limit=" + HISTORY_LIMIT);
    if (resp.status === 401) { handleUnauthorized(); return; }
    if (!resp.ok) { tableMessage(actionsBody, 2, "Failed to load the log (" + resp.status + ")", true); return; }
    const data = await resp.json();
    lastActions = data.actions || [];
    historyTotal = data.total ?? lastActions.length;
    renderActions();
  } catch (e) {
    tableMessage(actionsBody, 2, "Could not reach the server.", true);
  }
}

// Says what is on screen against what exists, so a truncated log does not
// read as the whole history.
function renderHistoryNote(shown) {
  const el = document.getElementById("history-note");
  if (!el) return;
  el.textContent = historyTotal > lastActions.length
    ? `showing ${shown} of the most recent ${lastActions.length} changes, out of ${historyTotal}`
    : `showing ${shown} of ${historyTotal} changes`;
}

function renderActions() {
  const rows = lastActions.filter((a) => matchesGroup(a.action, group));
  if (rows.length === 0) {
    renderHistoryNote(0);
    tableMessage(actionsBody, 2, group === "all"
      ? "Nothing recorded yet."
      : "Nothing recorded yet for this kind of change.", false);
    return;
  }
  actionsBody.innerHTML = "";
  renderHistoryNote(rows.length);
  for (const a of rows) {
    const row = document.createElement("tr");
    const tsTd = cell(formatStamp(a.ts));
    tsTd.className = "time";
    row.appendChild(tsTd);
    row.appendChild(cell(actionSentence(a)));
    actionsBody.appendChild(row);
  }
}

if (historyFilter) {
  historyFilter.addEventListener("click", (e) => {
    const btn = e.target.closest("button[data-group]");
    if (!btn) return;
    group = btn.dataset.group;
    for (const b of historyFilter.querySelectorAll("button")) {
      b.setAttribute("aria-pressed", String(b.dataset.group === group));
    }
    renderActions();
  });
}

function renderAdmins(admins) {
  if (admins.length === 0) { tableMessage(adminsBody, 4, "No admins configured.", false); return; }
  adminsBody.innerHTML = "";
  const me = getUsername();

  for (const a of admins) {
    const row = document.createElement("tr");

    const nameTd = document.createElement("td");
    const name = document.createElement("span");
    name.className = "admin-name";
    name.textContent = a.username;
    nameTd.appendChild(name);
    if (a.username === me) nameTd.appendChild(tag("You", "you"));
    if (a.disabled) nameTd.appendChild(tag("Disabled", "off"));
    row.appendChild(nameTd);

    const createdTd = cell(formatStamp(a.created_at));
    createdTd.className = "time";
    row.appendChild(createdTd);

    // null until they have signed in at least once.
    const seenTd = cell(a.last_login_at ? formatStamp(a.last_login_at) : "never");
    seenTd.className = a.last_login_at ? "time" : "time muted";
    row.appendChild(seenTd);

    const actTd = document.createElement("td");
    actTd.className = "row-actions";

    const pwBtn = document.createElement("button");
    pwBtn.className = "btn";
    pwBtn.textContent = "Reset password";
    pwBtn.addEventListener("click", () => openPasswordDialog(a.username, a.username === me));
    actTd.appendChild(pwBtn);

    // Disabling yourself ends your own session, and the API refuses to disable
    // the last active admin, so neither is offered as a button that would only
    // fail or log you out.
    if (!a.disabled && a.username !== me) {
      const disBtn = document.createElement("button");
      disBtn.className = "btn danger";
      disBtn.textContent = "Disable";
      disBtn.addEventListener("click", () => openDisableDialog(a.username));
      actTd.appendChild(disBtn);
    }

    row.appendChild(actTd);
    adminsBody.appendChild(row);
  }
}

// Both forms are built with createElement and set values as properties, never
// interpolated into innerHTML, for the same reason as the server form.
export function openAdminForm() {
  formContainer.innerHTML = "";
  const form = document.createElement("form");

  const title = document.createElement("h3");
  title.className = "form-title";
  title.textContent = "Add admin";
  form.appendChild(title);

  const user = field(form, "Username", "username", "text");
  const pass = field(form, "Password (at least 8 characters)", "password", "password");

  const errDiv = document.createElement("div");
  errDiv.id = "admin-error";
  form.appendChild(errDiv);

  form.appendChild(formActions("Create"));
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    errDiv.textContent = "";
    await submit(errDiv, ADMINS_URL, "POST", {
      username: user.value.trim(),
      password: pass.value,
    });
  });
  formContainer.appendChild(form);
}

// The consequence is spelled out rather than implied: resetting your own
// password signs you out, and resetting someone else's ends their sessions.
function openPasswordDialog(username, isSelf) {
  formDialog({
    title: "Reset password for " + username,
    body: isSelf
      ? "This signs you out of every session, including this one."
      : "This signs " + username + " out of every session.",
    label: "New password (at least 8 characters)",
    type: "password",
    submitLabel: "Reset password",
    onSubmit: async (value) => {
      // false means the session ended and the page is already back at sign-in;
      // returning normally lets the dialog close instead of sitting over it.
      if (await request(ADMINS_URL + "/" + encodeURIComponent(username) + "/password", "PUT", { password: value })) {
        loadAdmins();
      }
    },
  });
}

async function openDisableDialog(username) {
  const ok = await confirmDialog({
    title: "Disable " + username + "?",
    body: "They will be signed out of every session and will not be able to sign in again. The account is kept, so the change history still shows what they did.",
    confirmLabel: "Disable",
    danger: true,
  });
  if (!ok) return;
  try {
    if (await request(ADMINS_URL + "/" + encodeURIComponent(username) + "/disable", "PUT")) loadAdmins();
  } catch (e) {
    tableMessage(adminsBody, 4, e.message, true);
  }
}

function field(form, labelText, name, type) {
  const wrap = document.createElement("div");
  wrap.className = "field";
  const label = document.createElement("label");
  label.textContent = labelText;
  const input = document.createElement("input");
  input.type = type;
  input.name = name;
  if (type === "password") input.autocomplete = "new-password";
  wrap.appendChild(label);
  wrap.appendChild(input);
  form.appendChild(wrap);
  return input;
}

function formActions(submitText) {
  const actions = document.createElement("div");
  actions.className = "form-actions";
  const saveBtn = document.createElement("button");
  saveBtn.type = "submit";
  saveBtn.className = "btn primary";
  saveBtn.textContent = submitText;
  const cancelBtn = document.createElement("button");
  cancelBtn.type = "button";
  cancelBtn.className = "btn";
  cancelBtn.textContent = "Cancel";
  cancelBtn.addEventListener("click", () => { formContainer.innerHTML = ""; });
  actions.appendChild(saveBtn);
  actions.appendChild(cancelBtn);
  return actions;
}

// The API's own messages are shown as they are: they name the rule that was
// broken ("Password must be between 8 and 72 bytes"), which is more use than
// anything this could say instead.
//
// Resolves true on success. An expired session is not a form error, so it sends
// the page back to sign-in and resolves false; callers must not carry on.
async function request(url, method, body) {
  const opts = { method };
  if (body) {
    opts.headers = { "Content-Type": "application/json" };
    opts.body = JSON.stringify(body);
  }
  const resp = await authFetch(url, opts);
  if (resp.status === 401) { handleUnauthorized(); return false; }
  if (!resp.ok) {
    const data = await resp.json().catch(() => ({}));
    throw new Error(data.error || "Request failed");
  }
  return true;
}

async function submit(errEl, url, method, body) {
  try {
    if (!(await request(url, method, body))) { formContainer.innerHTML = ""; return; }
    formContainer.innerHTML = "";
    loadAdmins();
  } catch (e) { errEl.textContent = e.message; }
}