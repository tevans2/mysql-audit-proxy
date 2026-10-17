import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { statementKind, kindClass, KIND_TYPES } from "./stmtkind.js";

test("reads are the verbs that change nothing", () => {
  for (const k of ["SELECT", "SHOW", "EXPLAIN", "DESCRIBE", "DESC"]) {
    assert.equal(statementKind(k), "read", k);
  }
});

test("writes are the verbs that change rows", () => {
  for (const k of ["INSERT", "UPDATE", "DELETE", "REPLACE", "LOAD"]) {
    assert.equal(statementKind(k), "write", k);
  }
});

test("schema covers DDL and the privilege verbs", () => {
  for (const k of ["CREATE", "DROP", "ALTER", "TRUNCATE", "RENAME", "GRANT", "REVOKE"]) {
    assert.equal(statementKind(k), "schema", k);
  }
});

test("transaction and session control is other, not schema", () => {
  // These are the ones that would cry wolf if they were risk-coded red, and
  // the API leaves them out of kind=schema for the same reason.
  for (const k of ["BEGIN", "COMMIT", "ROLLBACK", "SET", "USE", "WITH", "CALL"]) {
    assert.equal(statementKind(k), "other", k);
  }
});

// The API's grouping lives in internal/store/store.go. Reading it here, rather
// than copying the lists into this file, is what makes the test able to fail
// when either side changes.
function apiKinds() {
  const src = readFileSync(new URL("../internal/store/store.go", import.meta.url), "utf8");
  const block = src.match(/var stmtKinds = map\[string\]\[\]string\{([\s\S]*?)\n\}/);
  assert.ok(block, "could not find stmtKinds in internal/store/store.go");
  const out = {};
  for (const m of block[1].matchAll(/"(\w+)":\s*\{([^}]*)\}/g)) {
    out[m[1]] = [...m[2].matchAll(/"(\w+)"/g)].map((x) => x[1]);
  }
  return out;
}

test("the read, write and schema tiers match the API's kind filter exactly", () => {
  const api = apiKinds();
  assert.deepEqual(Object.keys(api).sort(), ["read", "schema", "write"]);
  for (const kind of Object.keys(api)) {
    assert.deepEqual([...KIND_TYPES[kind]].sort(), [...api[kind]].sort(), kind);
  }
});

test("an unknown or missing keyword is other rather than a guess", () => {
  assert.equal(statementKind(""), "other");
  assert.equal(statementKind(null), "other");
  assert.equal(statementKind(undefined), "other");
  assert.equal(statementKind("WIBBLE"), "other");
});

test("case and surrounding space do not change the answer", () => {
  assert.equal(statementKind("select"), "read");
  assert.equal(statementKind("  Drop  "), "schema");
});

test("kindClass gives one class per tier", () => {
  assert.equal(kindClass("SELECT"), "kind-read");
  assert.equal(kindClass("UPDATE"), "kind-write");
  assert.equal(kindClass("ALTER"), "kind-schema");
  assert.equal(kindClass("COMMIT"), "kind-other");
});