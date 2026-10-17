// Modal dialogs built on the native <dialog>, so Esc, the backdrop and focus
// are the browser's job rather than ours. Shared by the admin and server
// screens; nothing here knows what it is confirming.

function shell(title, bodyText) {
  const dlg = document.createElement("dialog");
  const body = document.createElement("div");
  body.className = "dialog-body";

  const h = document.createElement("h2");
  h.textContent = title;
  body.appendChild(h);

  if (bodyText) {
    const p = document.createElement("p");
    p.textContent = bodyText;
    body.appendChild(p);
  }

  dlg.appendChild(body);
  return { dlg, body };
}

function actions(dlg) {
  const row = document.createElement("div");
  row.className = "dialog-actions";
  dlg.appendChild(row);
  return row;
}

function button(label, cls) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = cls;
  b.textContent = label;
  return b;
}

// Resolves true if confirmed, false for Cancel, Esc or the backdrop.
export function confirmDialog({ title, body, confirmLabel = "Confirm", danger = false }) {
  return new Promise((resolve) => {
    const { dlg } = shell(title, body);
    const row = actions(dlg);

    const ok = button(confirmLabel, danger ? "btn danger" : "btn primary");
    const cancel = button("Cancel", "btn");
    row.appendChild(ok);
    row.appendChild(cancel);

    let answer = false;
    ok.addEventListener("click", () => { answer = true; dlg.close(); });
    cancel.addEventListener("click", () => dlg.close());
    dlg.addEventListener("close", () => { dlg.remove(); resolve(answer); });

    document.body.appendChild(dlg);
    dlg.showModal();
    cancel.focus();
  });
}

// One field and a submit. onSubmit(value) may throw: the message is shown and
// the dialog stays open, so a rejected password does not lose what was typed.
export function formDialog({ title, body, label, type = "text", submitLabel = "Save", onSubmit }) {
  return new Promise((resolve) => {
    const { dlg, body: box } = shell(title, body);

    const form = document.createElement("form");
    form.method = "dialog";

    const field = document.createElement("div");
    field.className = "field";
    const lab = document.createElement("label");
    lab.textContent = label;
    const input = document.createElement("input");
    input.type = type;
    // A password asked for here is a new one, so keep password managers from
    // filling in the signed-in admin's own.
    if (type === "password") input.autocomplete = "new-password";
    field.appendChild(lab);
    field.appendChild(input);
    form.appendChild(field);

    const err = document.createElement("div");
    err.className = "dialog-error";
    form.appendChild(err);
    box.appendChild(form);

    const row = actions(dlg);
    const save = button(submitLabel, "btn primary");
    save.type = "submit";
    save.setAttribute("form", "");
    const cancel = button("Cancel", "btn");
    row.appendChild(save);
    row.appendChild(cancel);

    let done = false;
    const submit = async () => {
      err.textContent = "";
      save.disabled = true;
      try {
        await onSubmit(input.value);
        done = true;
        dlg.close();
      } catch (e) {
        err.textContent = e.message;
        save.disabled = false;
        input.focus();
      }
    };

    save.addEventListener("click", (e) => { e.preventDefault(); submit(); });
    form.addEventListener("submit", (e) => { e.preventDefault(); submit(); });
    cancel.addEventListener("click", () => dlg.close());
    dlg.addEventListener("close", () => { dlg.remove(); resolve(done); });

    document.body.appendChild(dlg);
    dlg.showModal();
    input.focus();
  });
}