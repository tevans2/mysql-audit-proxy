// Turns a row of the attribution log into a sentence, and says whether it is
// an admin change or a server one so the history can be filtered.

export const GROUPS = ["all", "admins", "servers"];

const ADMIN_ACTIONS = new Set(["CREATE_ADMIN", "CHANGE_PASSWORD", "DISABLE_ADMIN"]);
const SERVER_ACTIONS = new Set(["CREATE_SERVER", "UPDATE_SERVER", "DELETE_SERVER"]);

export function actionGroup(action) {
  const a = String(action || "").toUpperCase();
  if (ADMIN_ACTIONS.has(a)) return "admins";
  if (SERVER_ACTIONS.has(a)) return "servers";
  return "other";
}

export function matchesGroup(action, group) {
  return group === "all" || actionGroup(action) === group;
}

// The actor is always named, because the point of the log is who did it.
// An unknown action still reads as a sentence rather than disappearing
export function actionSentence(row) {
  const actor = (row && row.username) || "someone";
  const target = (row && row.target) || "";
  const action = String((row && row.action) || "").toUpperCase();
  const self = target && actor === target;

  switch (action) {
    case "CREATE_ADMIN":
      return `${actor} added the admin ${target}`;
    case "DISABLE_ADMIN":
      return `${actor} disabled the admin ${target}`;
    case "CHANGE_PASSWORD":
      return self
        ? `${actor} changed their own password`
        : `${actor} changed the password for ${target}`;
    case "CREATE_SERVER":
      return `${actor} added the server ${target}`;
    case "UPDATE_SERVER":
      return `${actor} edited the server ${target}`;
    case "DELETE_SERVER":
      return `${actor} removed the server ${target}`;
    default:
      return target
        ? `${actor} performed ${action || "an unknown action"} on ${target}`
        : `${actor} performed ${action || "an unknown action"}`;
  }
}