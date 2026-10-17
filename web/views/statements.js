// renders the statements table. Takes the total count as a parameter (rather than reaching into store.js directly) so
// this module has no dependency on store.js at all.

import { cell, badge, formatTime, stripLeadingComments, statementType } from '../format.js';
import { kindClass } from '../stmtkind.js';
import { renderPager } from './pager.js';
import { openSession } from './sessiondrawer.js';

const stmtBody = document.getElementById("stmt-body");
const stmtCount = document.getElementById("stmt-count");
const stmtPager = document.getElementById("stmt-pager");

// pageLength is how many rows the page held before the handshake filter ran,
// so the range reflects the API's own offset rather than what is on screen.
// onPage is store.js's setStmtOffset, passed in to keep this module free of
// a store dependency.
// sqlCell shows the statement without the comment a driver put in front of it,
// clamped to a couple of lines. Clicking (or Enter/Space) swaps in the full
// text exactly as it was received, comment included, so nothing is hidden from
// the audit view, only tucked away. textContent throughout: the SQL is
// attacker-influenced.
// The error sits under its statement: as a column it was empty on nearly every row.
//
// Which statements are expanded is remembered here, by statement id, because
// the table is rebuilt on every poll: state kept on the cell itself would be
// thrown away with it, collapsing whatever the operator was reading.
const expandedIds = new Set();

function sqlCell(sql, error, id) {
  const full = sql ?? "";
  const shown = stripLeadingComments(full) || full; // a comment-only statement shows as itself
  const td = document.createElement("td");
  td.className = "sql";
  td.tabIndex = 0;
  if (id != null) td.dataset.stmtId = String(id);
  const text = document.createElement("div");
  text.className = "sql-text";
  td.appendChild(text);

  if (error) {
    const err = document.createElement("span");
    err.className = "err-text";
    err.textContent = error; // attacker-influenced --> textContent
    td.appendChild(err);
  }

  let expanded = id != null && expandedIds.has(id);
  const show = () => {
    text.textContent = expanded ? full : shown;
    text.classList.toggle("expanded", expanded);
    td.title = expanded ? "Click to collapse" : "Click to show the full statement";
  };
  show();
  const toggle = () => {
    expanded = !expanded;
    if (id != null) { if (expanded) expandedIds.add(id); else expandedIds.delete(id); }
    show();
  };
  td.addEventListener("click", toggle);
  td.addEventListener("keydown", (e) => {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      toggle();
    }
  });
  return td;
}

// The connection id opens that session rather than being inert text: it is the
// most common question a statement raises ("what else did they run?").
function connCell(connId) {
  const td = document.createElement("td");
  td.className = "conn";
  const btn = document.createElement("button");
  btn.className = "linkish";
  btn.textContent = connId;
  btn.addEventListener("click", () => openSession(connId));
  td.appendChild(btn);
  return td;
}

export function renderStatements(statements, hidden = 0, total = statements.length, offset = 0, pageLength = statements.length, onPage = null, pageSize = 50) {
  // A keyboard user may be sitting on a statement when the poll rebuilds the
  // table; put focus back on the same one rather than dropping it to the page.
  const focusedId = document.activeElement && document.activeElement.closest
    ? (document.activeElement.closest("td.sql") || {}).dataset?.stmtId
    : undefined;

  stmtBody.innerHTML = "";
  for (const s of statements) {
    const row = document.createElement("tr");
    if (s.error) row.classList.add("failed"); // visually distinct failed row
    row.appendChild(cell(formatTime(s.ts), "time"));
    row.appendChild(connCell(s.conn_id));
    row.appendChild(cell(s.db_user));
    row.appendChild(cell(s.upstream));
    row.appendChild(cell(s.db || "—", "schema"));
    const typeTd = document.createElement("td");
    const type = statementType(s);
    typeTd.appendChild(badge(type, kindClass(type)));
    row.appendChild(typeTd);
    row.appendChild(sqlCell(s.sql, s.error, s.id));
    stmtBody.appendChild(row);
  }

  if (focusedId !== undefined) {
    for (const td of stmtBody.querySelectorAll("td.sql")) {
      if (td.dataset.stmtId === focusedId) { td.focus({ preventScroll: true }); break; }
    }
  }

  // Forget statements that have left the page once the set grows, so a long
  // session of expanding things does not accumulate ids forever.
  if (expandedIds.size > 200) {
    const onPage = new Set(statements.map((s) => s.id));
    for (const id of expandedIds) if (!onPage.has(id)) expandedIds.delete(id);
  }

  const range = pageLength === 0 ? "0" : `${offset + 1}-${offset + pageLength}`;
  stmtCount.textContent = hidden > 0
    ? `showing ${range} of ${total} statements · ${hidden} handshake hidden on this page`
    : `showing ${range} of ${total} statements`;
  if (onPage) renderPager(stmtPager, total, offset, pageSize, onPage);
}