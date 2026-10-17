// presentation helpers. No DOM queries beyond element creation,
// no app state. Safe to import from anywhere.

export function cell(text, className) {
  const td = document.createElement("td");
  td.textContent = text;
  if (className) td.className = className;
  return td;
}

export function badge(text, cls) {
  const span = document.createElement("span");
  span.className = "badge " + cls;
  span.textContent = text;
  return span;
}

export function formatTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  // Show milliseconds: sub-second ordering is load-bearing (the dedupe key is
  // conn_id+ts+sql), so second-resolution hides the true sequence.
  return d.toLocaleString() + "." + String(d.getMilliseconds()).padStart(3, "0");
}

export function badgeClass(type) {
  const known = ["SELECT", "UPDATE", "INSERT", "DELETE", "SHOW"];
  return known.includes(type) ? type : "OTHER";
}

// stripLeadingComments mirrors skipLeadingComments in
// internal/proxy/runtime/sql.go: it drops the whitespace and comments before a
// statement's first token (/* */, "-- " and #), so the list can show the
// statement and not the tag a driver put in front of it. Display only; the
// stored text is never altered.
//
// /*! and /*+ comments are kept: MySQL executes the first, and the second is
// an optimizer hint. An unterminated /* swallows the rest, as MySQL would.
export function stripLeadingComments(sql) {
  let s = sql ?? "";
  for (;;) {
    s = s.replace(/^[ \t\r\n]+/, "");
    if (s.startsWith("/*!") || s.startsWith("/*+")) return s;
    if (s.startsWith("/*")) {
      const end = s.indexOf("*/", 2);
      if (end === -1) return "";
      s = s.slice(end + 2);
    } else if (s.startsWith("#") || /^--([ \t\r\n]|$)/.test(s)) {
      const nl = s.indexOf("\n");
      s = nl === -1 ? "" : s.slice(nl + 1);
    } else {
      return s;
    }
  }
}

// statementType is the type to badge. Rows stored before the proxy skipped
// leading comments carry "/*" (or nothing) for a commented statement, so when
// the stored type is not a keyword, work it out from the SQL instead.
export function statementType(stmt) {
  const stored = stmt.stmt_type ?? "";
  if (/^[A-Za-z]/.test(stored)) return stored;
  const body = stripLeadingComments(stmt.sql);
  const m = /^[^\s(]+/.exec(body);
  return m ? m[0].toUpperCase() : stored;
}

