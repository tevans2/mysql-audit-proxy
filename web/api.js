//endpoints, signed-in flag, authenticated fetch, login, logout.

export const USE_LIVE_API = true;
// Same origin: the edge (deploy/Caddyfile) serves this page and forwards /api/*.
export const API_BASE = "";
export const CONN_URL = USE_LIVE_API ? API_BASE + "/api/connections" : "mock-connections.json";
export const STMT_URL = USE_LIVE_API ? API_BASE + "/api/statements" : "mock-statements.json";
export const SERVERS_URL = API_BASE + "/api/servers";
export const ADMINS_URL = API_BASE + "/api/admins";
export const ACTIONS_URL = API_BASE + "/api/actions";

/*
 * The session itself is an HttpOnly cookie the API sets at login: script cannot
 * read it, and the browser sends it by itself. All that is kept here is a flag
 * saying a sign-in happened, so a reload can show the app instead of the login
 * form. It is not a credential; if the cookie has expired the first request
 * answers 401 and handleUnauthorized() clears it.
 */
export function isSignedIn() {
  try { return localStorage.getItem("signed-in") === "1"; } catch (e) { return false; }
}
function setSignedIn() { try { localStorage.setItem("signed-in", "1"); } catch (e) {} }
export function clearSession() {
  try { 
    localStorage.removeItem("signed-in"); 
    localStorage.removeItem("username");
    localStorage.removeItem("view");
  } catch (e) {}
}

// Who the session belongs to, so the Admins screen can tell the signed-in
// admin apart from the others (disabling yourself ends your own session).
export function getUsername() { return localStorage.getItem("username"); }
export function setUsername(u) { localStorage.setItem("username", u); }

/*
 * Same-origin fetch with the session cookie. State-changing requests carry
 * X-Requested-With, which the API requires as its CSRF check: a cross-site
 * form post cannot set a custom header.
 */
export function authFetch(url, options = {}) {
  const method = (options.method || "GET").toUpperCase();
  const headers = Object.assign({}, options.headers);
  if (method !== "GET" && method !== "HEAD") headers["X-Requested-With"] = "XMLHttpRequest";
  return fetch(url, Object.assign({}, options, { headers, credentials: "same-origin" }));
}

// The status travels with the error so the sign-in screen can tell a wrong
// password from a lockout, which read very differently to the operator.
export async function login(username, password) {
  const resp = await fetch(API_BASE + "/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ username, password }),
  });
  if (!resp.ok) {
    const err = new Error("Invalid username or password");
    err.status = resp.status;
    throw err;
  }
  const data = await resp.json();
  setSignedIn();
  setUsername(data.username || username);
}

// Ends the session on the server (deletes the row, clears the cookie). The
// local flag is cleared whether or not the request got through, so the user is
// always signed out of the page.
export async function logout() {
  try { await authFetch(API_BASE + "/api/logout", { method: "POST" }); } catch (e) {}
  clearSession();
}

export function fetchServers() { return authFetch(SERVERS_URL); }

// Only parameters with a value are sent, so an unset filter is absent from
// the query string rather than an empty string the API would have to decide
// how to interpret.
export function buildQuery(params) {
  const q = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === "") continue;
    q.set(key, value);
  }
  const s = q.toString();
  return s ? "?" + s : "";
}

// Mock mode reads fixed JSON files, so filters are only appended in live
// mode; main.js hides the filter controls when USE_LIVE_API is false.
export function fetchConnections(filters = {}) {
  return authFetch(CONN_URL + (USE_LIVE_API ? buildQuery(filters) : ""));
}

export function fetchStatements(filters = {}) {
  return authFetch(STMT_URL + (USE_LIVE_API ? buildQuery(filters) : ""));
}