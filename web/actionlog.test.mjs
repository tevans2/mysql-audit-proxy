import test from "node:test";
import assert from "node:assert/strict";
import { actionSentence, actionGroup, matchesGroup } from "./actionlog.js";

test("admin actions read as sentences", () => {
  assert.equal(
    actionSentence({ username: "admin", action: "CREATE_ADMIN", target: "maria" }),
    "admin added the admin maria",
  );
  assert.equal(
    actionSentence({ username: "admin", action: "DISABLE_ADMIN", target: "dave" }),
    "admin disabled the admin dave",
  );
});

test("changing your own password reads differently from changing someone else's", () => {
  assert.equal(
    actionSentence({ username: "dave", action: "CHANGE_PASSWORD", target: "dave" }),
    "dave changed their own password",
  );
  assert.equal(
    actionSentence({ username: "dave", action: "CHANGE_PASSWORD", target: "admin" }),
    "dave changed the password for admin",
  );
});

test("server actions read as sentences", () => {
  assert.equal(
    actionSentence({ username: "admin", action: "CREATE_SERVER", target: "db-a" }),
    "admin added the server db-a",
  );
  assert.equal(
    actionSentence({ username: "admin", action: "UPDATE_SERVER", target: "db-a" }),
    "admin edited the server db-a",
  );
  assert.equal(
    actionSentence({ username: "admin", action: "DELETE_SERVER", target: "db-b" }),
    "admin removed the server db-b",
  );
});

test("an action the UI has not been taught still reads as a sentence", () => {
  assert.equal(
    actionSentence({ username: "admin", action: "ENABLE_ADMIN", target: "maria" }),
    "admin performed ENABLE_ADMIN on maria",
  );
  assert.equal(
    actionSentence({ username: "admin", action: "SOMETHING", target: "" }),
    "admin performed SOMETHING",
  );
});

test("a row missing its actor still names someone", () => {
  assert.equal(
    actionSentence({ action: "CREATE_ADMIN", target: "maria" }),
    "someone added the admin maria",
  );
  assert.equal(actionSentence({}), "someone performed an unknown action");
  assert.equal(actionSentence(null), "someone performed an unknown action");
});

test("actions split into the two groups the filter offers", () => {
  for (const a of ["CREATE_ADMIN", "CHANGE_PASSWORD", "DISABLE_ADMIN"]) {
    assert.equal(actionGroup(a), "admins", a);
  }
  for (const a of ["CREATE_SERVER", "UPDATE_SERVER", "DELETE_SERVER"]) {
    assert.equal(actionGroup(a), "servers", a);
  }
  assert.equal(actionGroup("WIBBLE"), "other");
  assert.equal(actionGroup(""), "other");
});

test("the all filter keeps everything, including actions in neither group", () => {
  assert.equal(matchesGroup("CREATE_ADMIN", "all"), true);
  assert.equal(matchesGroup("WIBBLE", "all"), true);
  assert.equal(matchesGroup("CREATE_ADMIN", "admins"), true);
  assert.equal(matchesGroup("CREATE_ADMIN", "servers"), false);
  assert.equal(matchesGroup("WIBBLE", "admins"), false);
});