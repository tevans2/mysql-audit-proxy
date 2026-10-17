// Run with `node --test "web/**/*.test.mjs"` (make web-test). Not served: .dockerignore keeps
// it out of the edge image.
import test from "node:test";
import assert from "node:assert/strict";
import { stripLeadingComments, statementType } from "./format.js";

test("strips the tags DBeaver and Connector/J put in front of a statement", () => {
  assert.equal(
    stripLeadingComments("/* ApplicationName=DBeaver 26.2.0 - SQLEditor <demo-2.sql> */ SELECT id, name FROM sales.customers LIMIT 0, 200"),
    "SELECT id, name FROM sales.customers LIMIT 0, 200");
  assert.equal(
    stripLeadingComments("/* mysql-connector-j-8.2.0 (Revision: 06a1f724) */SELECT @@session.auto_increment_increment"),
    "SELECT @@session.auto_increment_increment");
});

test("skips several comments and every comment style", () => {
  assert.equal(stripLeadingComments("/* a */ /* b */\n-- c\n# d\nDELETE FROM t"), "DELETE FROM t");
});

test("leaves comments that are not leading, and string contents, alone", () => {
  const q = "SELECT /* x */ 'a /* not a comment */ b' -- trailing";
  assert.equal(stripLeadingComments(q), q);
});

test("-- needs a following space, and a minus is not a comment", () => {
  assert.equal(stripLeadingComments("--1 SELECT"), "--1 SELECT");
  assert.equal(stripLeadingComments("-1"), "-1");
  assert.equal(stripLeadingComments("--"), "");
});

test("comment-only and unterminated comments strip to nothing", () => {
  assert.equal(stripLeadingComments("/* only a comment */"), "");
  assert.equal(stripLeadingComments("/* never closed SELECT 1"), "");
  assert.equal(stripLeadingComments(null), "");
});

test("executable and hint comments are kept", () => {
  assert.equal(stripLeadingComments("/*!40101 SET NAMES utf8 */"), "/*!40101 SET NAMES utf8 */");
  assert.equal(stripLeadingComments("/*+ SET_VAR(a=1) */ SELECT 1"), "/*+ SET_VAR(a=1) */ SELECT 1");
});

test("statementType trusts a stored keyword", () => {
  assert.equal(statementType({ stmt_type: "SELECT", sql: "SELECT 1" }), "SELECT");
});

test("statementType repairs rows stored as \"/*\" before the proxy fix", () => {
  assert.equal(statementType({ stmt_type: "/*", sql: "/* ApplicationName=DBeaver */ SET autocommit=1" }), "SET");
  assert.equal(statementType({ stmt_type: "/*", sql: "/* tag */ show databases" }), "SHOW");
  assert.equal(statementType({ stmt_type: "", sql: "/* tag */ SELECT(1)" }), "SELECT");
});

test("statementType falls back to what was stored when there is nothing to derive", () => {
  assert.equal(statementType({ stmt_type: "/*", sql: "/* only a comment */" }), "/*");
  assert.equal(statementType({ sql: "" }), "");
});
