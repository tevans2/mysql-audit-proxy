// server/port configuration management: list, add, edit, delete. Live-data only (no mock mode).
// Only loadServers and openServerForm are called from outside this module.

import { authFetch, SERVERS_URL } from '../api.js';
import { handleUnauthorized } from '../store.js';
import { configState } from '../configstate.js';
import { confirmDialog } from './dialog.js';

const serversList = document.getElementById("servers-body");
const formContainer = document.getElementById("server-form-container");

// What the cards on screen were drawn from. The list is refreshed every few
// seconds, and rebuilding it each time throws away keyboard focus and can eat a
// click that lands mid-rebuild, so an unchanged list is left alone.
let drawn = null;

function serversMessage(text, isError) {
  drawn = null;
  serversList.innerHTML = "";
  const p = document.createElement("p");
  p.className = isError ? "list-msg err-text" : "list-msg";
  p.textContent = text;
  serversList.appendChild(p);
}

const configStatus = document.getElementById("config-status");

// Names what the proxy is actually running, against what is saved. The
// timestamp matters as much as the numbers: a proxy that stopped an hour ago
// on the right revision looks identical to a healthy one without it.
// The four states come from configstate.js, shared with the header pill so
// the two cannot disagree.
function renderConfigStatus(revision, appliedRevision, appliedAt) {
  if (!configStatus) return;
  configStatus.innerHTML = "";
  const state = configState({ revision, appliedRevision, appliedAt });
  if (!state) { configStatus.hidden = true; return; }
  configStatus.hidden = false;
  configStatus.className = "config-banner " + state.state;

  const dot = document.createElement("span");
  dot.className = "dot";
  dot.setAttribute("aria-hidden", "true");
  configStatus.appendChild(dot);

  const reported = appliedAt ? new Date(appliedAt) : null;
  const pairs = [
    ["Saved", "revision " + revision],
    ["Proxy running", appliedRevision ? "revision " + appliedRevision : "not reported"],
    ["Last report", reported && !isNaN(reported) ? reported.toLocaleTimeString() : "never"],
  ];
  for (const [label, value] of pairs) {
    const item = document.createElement("span");
    item.className = "banner-item";
    const l = document.createElement("span");
    l.className = "banner-label";
    l.textContent = label;
    const v = document.createElement("span");
    v.className = "banner-value";
    v.textContent = value;
    item.appendChild(l);
    item.appendChild(v);
    configStatus.appendChild(item);
  }
}

export async function loadServers() {
  try {
    const resp = await authFetch(SERVERS_URL);
    if (resp.status === 401) { handleUnauthorized(); return; }
    if (!resp.ok) { serversMessage("Failed to load servers (" + resp.status + ")", true); return; }
    const data = await resp.json();
    renderConfigStatus(data.revision, data.applied_revision, data.applied_at);
    const servers = data.servers || [];
    if (servers.length === 0) { serversMessage("No servers configured.", false); return; }
    const sig = JSON.stringify(servers);
    if (sig === drawn) return;
    drawn = sig;
    renderServers(servers);
  } catch (e) {
    serversMessage("Could not reach the server.", true);
  }
}

function meta(dl, label, value) {
  const dt = document.createElement("dt");
  dt.textContent = label;
  const dd = document.createElement("dd");
  dd.textContent = value;
  dl.appendChild(dt);
  dl.appendChild(dd);
}

// One card per server, with the route drawn as a route: what a client connects
// to on the left, what the proxy forwards it to on the right.
function renderServers(servers) {
  serversList.innerHTML = "";
  for (const s of servers) {
    const card = document.createElement("article");
    card.className = s.enabled ? "server-card" : "server-card off";

    const head = document.createElement("div");
    head.className = "server-head";
    const name = document.createElement("h2");
    name.textContent = s.name;
    head.appendChild(name);

    const state = document.createElement("span");
    state.className = s.enabled ? "tag on" : "tag off";
    state.textContent = s.enabled ? "Accepting connections" : "Not accepting";
    head.appendChild(state);
    card.appendChild(head);

    const route = document.createElement("div");
    route.className = "route";
    for (const [label, value] of [
      ["Clients connect to", ":" + s.listen_port],
      ["Forwards to", s.upstream_host + ":" + s.upstream_port],
    ]) {
      const end = document.createElement("div");
      end.className = "route-end";
      const l = document.createElement("span");
      l.className = "route-label";
      l.textContent = label;
      const v = document.createElement("span");
      v.className = "route-value";
      v.textContent = value;
      end.appendChild(l);
      end.appendChild(v);
      if (route.children.length) {
        const arrow = document.createElement("span");
        arrow.className = "route-arrow";
        arrow.setAttribute("aria-hidden", "true");
        arrow.textContent = "→";
        route.appendChild(arrow);
      }
      route.appendChild(end);
    }
    card.appendChild(route);

    const dl = document.createElement("dl");
    dl.className = "server-meta";
    meta(dl, "User", s.upstream_user);
    meta(dl, "TLS", s.tls || "verify");
    meta(dl, "CA file", s.tls_ca_file || "System trust roots");
    meta(dl, "Password", s.has_upstream_password ? "set" : "none");
    card.appendChild(dl);

    const actions = document.createElement("div");
    actions.className = "row-actions";
    const editBtn = document.createElement("button");
    editBtn.className = "btn"; editBtn.textContent = "Edit";
    editBtn.addEventListener("click", () => openServerForm(s));
    const delBtn = document.createElement("button");
    delBtn.className = "btn danger"; delBtn.textContent = "Delete";
    delBtn.addEventListener("click", () => deleteServer(s));
    actions.appendChild(editBtn); actions.appendChild(delBtn);
    card.appendChild(actions);

    serversList.appendChild(card);
  }
}

// Build the add/edit form with createElement - values set as properties,
// never interpolated into innerHTML (avoids stored XSS from server names).
export function openServerForm(server) {
  const editing = !!server;
  const s = server || { name: "", listen_port: "", upstream_host: "", upstream_port: 3306, upstream_user: "", enabled: true };
  formContainer.innerHTML = "";

  const form = document.createElement("form");

  const title = document.createElement("h3");
  title.className = "form-title";
  title.textContent = editing ? "Edit server" : "Add server";
  form.appendChild(title);

  function field(labelText, name, type, value) {
    const wrap = document.createElement("div");
    wrap.className = "field";
    const label = document.createElement("label");
    label.textContent = labelText;
    const input = document.createElement("input");
    input.type = type;
    input.name = name;
    if (type === "password") input.autocomplete = "new-password"; // the upstream's, not the admin's
    if (value !== undefined && value !== null) input.value = value; // safe: property, not HTML
    wrap.appendChild(label);
    wrap.appendChild(input);
    form.appendChild(wrap);
    return input;
  }

  field("Name", "name", "text", s.name);
  field("Listen port", "listen_port", "number", s.listen_port);
  field("Upstream host", "upstream_host", "text", s.upstream_host);
  field("Upstream port", "upstream_port", "number", s.upstream_port);
  field("Upstream user", "upstream_user", "text", s.upstream_user);
  field("Upstream password" + (editing ? " (leave blank to keep current)" : ""), "upstream_pass", "password", "");

  const tlsWrap = document.createElement("div");
  tlsWrap.className = "field";
  const tlsLabel = document.createElement("label");
  tlsLabel.textContent = "Upstream TLS";
  const tlsSelect = document.createElement("select");
  tlsSelect.name = "tls";
  for (const [value, text] of [["verify", "Verify certificate (default)"], ["skip-verify", "Skip verification (development only)"]]) {
    const option = document.createElement("option");
    option.value = value; option.textContent = text;
    tlsSelect.appendChild(option);
  }
  tlsSelect.value = s.tls || "verify";
  tlsWrap.appendChild(tlsLabel); tlsWrap.appendChild(tlsSelect); form.appendChild(tlsWrap);
  const tlsWarning = document.createElement("p");
  tlsWarning.className = "field-warning";
  tlsWarning.textContent = "Development only: encryption remains enabled, but the database identity is not verified.";
  const updateWarning = () => { tlsWarning.hidden = tlsSelect.value !== "skip-verify"; };
  tlsSelect.addEventListener("change", updateWarning); updateWarning();
  form.appendChild(tlsWarning);
  field("CA file path inside the proxy (blank: system trust roots)", "tls_ca_file", "text", s.tls_ca_file || "");

  const enWrap = document.createElement("div");
  enWrap.className = "field";
  const enLabel = document.createElement("label");
  const enInput = document.createElement("input");
  enInput.type = "checkbox";
  enInput.name = "enabled";
  enInput.checked = !!s.enabled;
  enLabel.appendChild(enInput);
  enLabel.appendChild(document.createTextNode(" Enabled"));
  enWrap.appendChild(enLabel);
  form.appendChild(enWrap);

  const errDiv = document.createElement("div");
  errDiv.id = "server-error";
  form.appendChild(errDiv);

  const actions = document.createElement("div");
  actions.className = "form-actions";
  const saveBtn = document.createElement("button");
  saveBtn.type = "submit";
  saveBtn.className = "btn primary";
  saveBtn.textContent = editing ? "Save" : "Create";
  const cancelBtn = document.createElement("button");
  cancelBtn.type = "button";
  cancelBtn.className = "btn";
  cancelBtn.textContent = "Cancel";
  cancelBtn.addEventListener("click", () => { formContainer.innerHTML = ""; });
  actions.appendChild(saveBtn);
  actions.appendChild(cancelBtn);
  form.appendChild(actions);

  form.addEventListener("submit", (e) => { e.preventDefault(); saveServer(form, editing ? s.id : null); });
  formContainer.appendChild(form);
  form.name.focus();
}

async function saveServer(form, id) {
  const errEl = form.querySelector("#server-error");
  errEl.textContent = "";
  const portField = form.listen_port;
  portField.classList.remove("bad");
  const body = {
    name: form.name.value.trim(),
    listen_port: parseInt(form.listen_port.value, 10),
    upstream_host: form.upstream_host.value.trim(),
    upstream_port: parseInt(form.upstream_port.value, 10),
    upstream_user: form.upstream_user.value.trim(),
    upstream_pass: form.upstream_pass.value,
    enabled: form.enabled.checked,
    tls: form.elements.namedItem("tls").value,
    tls_ca_file: form.elements.namedItem("tls_ca_file").value.trim(),
  };
  if (id != null) body.id = id;
  try {
    const resp = await authFetch(SERVERS_URL, {
      method: id != null ? "PUT" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    // An expired session is not a form error: back to sign-in, as everywhere else.
    if (resp.status === 401) { formContainer.innerHTML = ""; handleUnauthorized(); return; }
    if (!resp.ok) {
      const data = await resp.json().catch(() => ({}));
      throw new Error(data.error || "Save failed");
    }
    formContainer.innerHTML = "";
    loadServers();
  } catch (e) {
    errEl.textContent = e.message;
    // A port clash is the one failure with an obvious field to point at.
    if (/port/i.test(e.message)) { portField.classList.add("bad"); portField.focus(); }
  }
}

async function deleteServer(s) {
  const ok = await confirmDialog({
    title: "Delete " + s.name + "?",
    body: "Clients will no longer be able to connect on port " + s.listen_port
      + ". Audit records of what went through it are kept.",
    confirmLabel: "Delete server",
    danger: true,
  });
  if (!ok) return;
  try {
    const resp = await authFetch(SERVERS_URL + "?id=" + encodeURIComponent(s.id), { method: "DELETE" });
    if (resp.status === 401) { handleUnauthorized(); return; }
    if (!resp.ok && resp.status !== 404) {
      const data = await resp.json().catch(() => ({}));
      serversMessage(data.error || "Delete failed", true);
      return;
    }
    loadServers();
  } catch (e) { serversMessage("Could not reach the server.", true); }
}