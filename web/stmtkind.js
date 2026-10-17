// Risk grouping for a statement's leading keyword, so the log can be skimmed
// for the things worth noticing rather than read verb by verb.

// These three must match stmtKinds in internal/store/store.go exactly: the
// API's kind= filter and the schema_changes count in GET /api/summary use that
// map, and a badge that disagrees with the filter on the same row is a bug.
// stmtkind.test.mjs reads the Go source and fails if they differ. The ones left
// out (BEGIN, COMMIT, SET, USE, WITH, CALL...) are "other" on purpose.
const READ = new Set(["SELECT", "SHOW", "EXPLAIN", "DESCRIBE", "DESC"]);

const WRITE = new Set(["INSERT", "UPDATE", "DELETE", "REPLACE", "LOAD"]);

// DDL plus the privilege verbs.
const SCHEMA = new Set([
  "CREATE", "ALTER", "DROP", "TRUNCATE", "RENAME",
  "GRANT", "REVOKE",
]);

// Exposed for the test that compares them with the API's.
export const KIND_TYPES = { read: [...READ], write: [...WRITE], schema: [...SCHEMA] };
 
export const KINDS = ["read", "write", "schema", "other"];
 
// Takes stmt_type as the API stores it (the leading keyword, uppercased) and
// returns one of KINDS. Anything unrecognised is "other" rather than a guess.
export function statementKind(stmtType) {
  if (!stmtType) return "other";
  const k = String(stmtType).trim().toUpperCase();
  if (READ.has(k)) return "read";
  if (WRITE.has(k)) return "write";
  if (SCHEMA.has(k)) return "schema";
  return "other";
}
 
// The class the badge carries. Kept next to the classification so the two
// cannot drift, and so styles.css has one set of names to define.
export function kindClass(stmtType) {
  return "kind-" + statementKind(stmtType);
}