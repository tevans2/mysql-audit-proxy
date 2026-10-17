// renders the connections table; a row click opens the session drawer, and the
// drawer is what offers to filter the log to that session.

import { cell, badge, formatTime } from '../format.js';
import { setConnOffset, setFilter, PAGE_SIZE } from '../store.js';
import { renderPager } from './pager.js';
import { openSession } from './sessiondrawer.js';

const connBody = document.getElementById("conn-body");
const connCount = document.getElementById("conn-count");
const connPager = document.getElementById("conn-pager");
const selectedConn = document.getElementById("selected-conn");

// The id is a real button, so the session can be opened from the keyboard; the
// row click stays as a larger target for the mouse. Stopping the click here
// keeps it from opening the drawer twice.
function connCell(connId) {
  const td = document.createElement("td");
  td.className = "conn";
  const btn = document.createElement("button");
  btn.className = "linkish";
  btn.textContent = connId;
  btn.addEventListener("click", (e) => { e.stopPropagation(); openSession(connId); });
  td.appendChild(btn);
  return td;
}

export function renderConnections(connections, total = connections.length, offset = 0) {
  connBody.innerHTML = "";
  for (const c of connections) {
    const row = document.createElement("tr");
    row.dataset.connId = c.conn_id;
    if (c.auth_result === "fail") row.classList.add("failed");
    row.appendChild(connCell(c.conn_id));
    row.appendChild(cell(c.db_user));
    row.appendChild(cell(c.client_addr, "time"));
    row.appendChild(cell(c.upstream));
    const authTd = document.createElement("td");
    authTd.appendChild(badge(c.auth_result, c.auth_result));
    if (c.auth_result === "fail" && c.auth_fail_reason) {
      const reason = document.createElement("span");
      reason.className = "err-text";
      reason.textContent = " " + c.auth_fail_reason; // via textContent, safe
      authTd.appendChild(reason);
    }
    row.appendChild(authTd);
    row.appendChild(cell(formatTime(c.ts_connect), "time"));
    row.appendChild(cell(formatTime(c.ts_disconnect), "time"));
    row.appendChild(cell(c.statement_count));
    row.addEventListener("click", () => openSession(c.conn_id));
    connBody.appendChild(row);
  }
  connCount.textContent = total === connections.length
    ? `${total} connections`
    : `${offset + 1}-${offset + connections.length} of ${total} connections`;
  renderPager(connPager, total, offset, PAGE_SIZE, setConnOffset);
}

// The selected connection is now an exact conn_id from the store, not a
// typed substring, so this compares whole values.
export function highlightSelected(connId) {
  for (const row of connBody.children) {
    row.classList.toggle("selected", connId !== "" && row.dataset.connId === connId);
  }
}

// Names the connection the statements are filtered to. The row highlight
// alone isn't enough: that row may be on another page, or filtered out of
// the connections table entirely.
export function renderSelectedConn(connId) {
  if (!selectedConn) return;
  selectedConn.innerHTML = "";
  if (!connId) return;

  const label = document.createElement("span");
  label.textContent = "filtered to " + connId + " ";

  const clear = document.createElement("button");
  clear.className = "clear";
  clear.textContent = "×";
  clear.title = "Clear connection filter";
  clear.addEventListener("click", () => setFilter("conn_id", ""));

  selectedConn.appendChild(label);
  selectedConn.appendChild(clear);
}