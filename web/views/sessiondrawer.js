// One session and everything that happened in it, in a slide-over.
// GET /api/connections/{id} returns the connection and its statements together, so this is one request.

import { authFetch, CONN_URL } from '../api.js';
import { formatTime, stripLeadingComments, statementType, badge } from '../format.js';
import { kindClass } from '../stmtkind.js';
import { handleUnauthorized, selectConnection } from '../store.js';

let open = null;

function row(dl, label, value) {
  const wrap = document.createElement("div");
  const dt = document.createElement("dt");
  dt.textContent = label;
  const dd = document.createElement("dd");
  dd.textContent = value;
  wrap.appendChild(dt);
  wrap.appendChild(dd);
  dl.appendChild(wrap);
}

export async function openSession(connId) {
  if (open) open.close();

  const dlg = document.createElement("dialog");
  dlg.className = "drawer";
  dlg.setAttribute("aria-label", "Session details");

  const head = document.createElement("div");
  head.className = "drawer-head";
  const heading = document.createElement("h2");
  heading.textContent = "Session";
  const sub = document.createElement("p");
  sub.className = "drawer-sub";
  sub.textContent = connId;
  const headText = document.createElement("div");
  headText.appendChild(heading);
  headText.appendChild(sub);
  head.appendChild(headText);

  const closeBtn = document.createElement("button");
  closeBtn.className = "btn drawer-close";
  closeBtn.setAttribute("aria-label", "Close session details");
  closeBtn.textContent = "×";
  closeBtn.addEventListener("click", () => dlg.close());
  head.appendChild(closeBtn);
  dlg.appendChild(head);

  const body = document.createElement("div");
  body.className = "drawer-body";
  const loading = document.createElement("p");
  loading.className = "list-msg";
  loading.textContent = "Loading…";
  body.appendChild(loading);
  dlg.appendChild(body);

  const foot = document.createElement("div");
  foot.className = "drawer-foot";
  const filterBtn = document.createElement("button");
  filterBtn.className = "btn primary";
  filterBtn.textContent = "Filter the log to this session";
  filterBtn.addEventListener("click", () => { selectConnection(connId); dlg.close(); });
  foot.appendChild(filterBtn);
  dlg.appendChild(foot);

  open = dlg;
  dlg.addEventListener("close", () => { dlg.remove(); if (open === dlg) open = null; });
  document.body.appendChild(dlg);
  dlg.showModal();
  closeBtn.focus();

  try {
    const resp = await authFetch(CONN_URL + "/" + encodeURIComponent(connId));
    if (resp.status === 401) { dlg.close(); handleUnauthorized(); return; }
    if (!resp.ok) { loading.textContent = "Could not load this session (" + resp.status + ")."; return; }
    const data = await resp.json();
    render(heading, sub, body, data.connection || {}, data.statements || []);
  } catch (e) {
    loading.textContent = "Could not reach the server.";
  }
}

function render(heading, sub, body, c, statements) {
  heading.textContent = (c.db_user || "Session") + " on " + (c.upstream || "?");

  if (c.auth_result === "fail") heading.appendChild(badge("Login rejected", "fail"));
  else if (!c.ts_disconnect) heading.appendChild(badge("Still connected", "ok"));

  sub.textContent = "Session " + c.conn_id + " · started " + formatTime(c.ts_connect);

  body.innerHTML = "";

  const dl = document.createElement("dl");
  dl.className = "drawer-meta";
  row(dl, "Database user", c.db_user || "—");
  row(dl, "Client address", c.client_addr || "—");
  row(dl, "Server", (c.upstream || "—") + (c.upstream_addr ? " → " + c.upstream_addr : ""));
  row(dl, "Proxy port", c.proxy_port != null ? String(c.proxy_port) : "—");
  row(dl, "Connected", formatTime(c.ts_connect));
  row(dl, "Disconnected", c.ts_disconnect ? formatTime(c.ts_disconnect) : "still connected");
  body.appendChild(dl);

  if (c.auth_result === "fail" && c.auth_fail_reason) {
    const why = document.createElement("p");
    why.className = "err-text";
    why.textContent = c.auth_fail_reason;
    body.appendChild(why);
  }

  // The count is the session's real total, not the length of what came back:
  // the API returns at most the newest 500, and a longer session would
  // otherwise read as having run exactly that many.
  const total = c.statement_count ?? statements.length;
  const h3 = document.createElement("h3");
  h3.className = "drawer-section";
  h3.textContent = "What happened · " + total + (total === 1 ? " statement" : " statements");
  body.appendChild(h3);

  if (statements.length < total) {
    const cut = document.createElement("p");
    cut.className = "list-msg";
    cut.textContent = "Showing the latest " + statements.length + " of " + total
      + ". Use \"Filter the log to this session\" to page through all of them.";
    body.appendChild(cut);
  }

  if (statements.length === 0) {
    const none = document.createElement("p");
    none.className = "list-msg";
    none.textContent = c.auth_result === "fail"
      ? "The login was rejected, so nothing ran."
      : "Nothing ran in this session.";
    body.appendChild(none);
    return;
  }

  // Oldest first: this reads as a story of the session, not as a log tail.
  // Handshake statements are left in, unlike the main table
  const ol = document.createElement("ol");
  ol.className = "timeline";
  for (const s of statements.slice().reverse()) {
    const li = document.createElement("li");

    const time = document.createElement("span");
    time.className = "timeline-time";
    time.textContent = formatTime(s.ts);
    li.appendChild(time);

    const main = document.createElement("div");
    const kind = statementType(s);
    main.appendChild(badge(kind, kindClass(kind)));
    if (s.db) {
      const db = document.createElement("span");
      db.className = "timeline-db";
      db.textContent = s.db;
      main.appendChild(db);
    }
    const code = document.createElement("code");
    code.textContent = stripLeadingComments(s.sql ?? "") || (s.sql ?? "");
    main.appendChild(code);
    if (s.error) {
      const err = document.createElement("span");
      err.className = "err-text";
      err.textContent = s.error;
      main.appendChild(err);
    }
    li.appendChild(main);
    ol.appendChild(li);
  }
  body.appendChild(ol);
}