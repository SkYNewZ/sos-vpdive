// sos-vpdive: the only script of both sites (spec §9.1). It enhances pages
// that work without it; only the anti-robot check needs JavaScript.
"use strict";

// Member form: show the fields of the chosen category only. Hidden
// fieldsets are disabled too, so that their required fields never block
// sending, and their values are not sent.
(() => {
  const select = document.querySelector("[data-category-select]");
  if (!select) return;
  const sync = () => {
    for (const fieldset of document.querySelectorAll("fieldset[data-category]")) {
      const chosen = fieldset.dataset.category === select.value;
      fieldset.hidden = !chosen;
      fieldset.disabled = !chosen;
    }
  };
  select.addEventListener("change", sync);
  sync();
})();

// Member form: the text being typed stays on this device until the request
// leaves (spec §9.6), with the form key of §3.2, so that sending again after
// a lost answer finds the same request. Never the screenshots, the
// anti-robot token or the screen 2 token. Seven days at most.
const DRAFT = "sos-vpdive-brouillon";
const DRAFT_MAX_AGE = 7 * 24 * 3600 * 1000;
const NOT_KEPT = new Set(["captures", "site_web", "cf-turnstile-response"]);

const readDraft = () => {
  try {
    const draft = JSON.parse(localStorage.getItem(DRAFT) || "null");
    if (draft && Date.now() - draft.savedAt < DRAFT_MAX_AGE) return draft;
    localStorage.removeItem(DRAFT);
  } catch {
    // Storage unavailable or unreadable: no draft.
  }
  return null;
};

// writeDraft stores draft, or removes it when null; false when storage fails.
const writeDraft = (draft) => {
  try {
    if (draft) localStorage.setItem(DRAFT, JSON.stringify(draft));
    else localStorage.removeItem(DRAFT);
    return true;
  } catch {
    return false;
  }
};

// After sending, or after « Ça règle mon problème », nothing is kept.
if (document.querySelector("[data-draft-done]")) writeDraft(null);

// Screen 2: the server holds the form key now. The text stays, the key goes,
// so text edited later never brings back this stored draft.
if (document.querySelector("[data-draft-key-used]")) {
  const draft = readDraft();
  if (draft) {
    delete draft.fields.cle;
    writeDraft(draft);
  }
}

const draftForm = document.querySelector("form[data-draft]");
const draftNote = document.querySelector("[data-draft-note]");
// Writing the draft back as it is tells whether storage works at all.
if (draftForm && draftNote && writeDraft(readDraft())) {
  const serverKey = draftForm.elements.cle.value;
  const kept = () =>
    [...draftForm.elements].filter((el) => el.name && !NOT_KEPT.has(el.name) && !["file", "submit", "button"].includes(el.type));
  const showCategory = () => draftForm.querySelector("[data-category-select]")?.dispatchEvent(new Event("change"));
  const draft = readDraft();
  if (draft) {
    // A page sent back with remarks keeps what the server filled in.
    for (const el of kept()) {
      if (el.name in draft.fields && (el.name === "cle" || el.value === "")) el.value = draft.fields[el.name];
    }
    showCategory();
  }
  const save = () => writeDraft({ savedAt: Date.now(), fields: Object.fromEntries(kept().map((el) => [el.name, el.value])) });
  draftForm.addEventListener("input", save);
  draftForm.addEventListener("change", save);
  // The key that leaves with the form is the one a lost answer must find again.
  draftForm.addEventListener("submit", save);
  draftNote.hidden = false;
  draftNote.querySelector("[data-draft-clear]").addEventListener("click", () => {
    writeDraft(null);
    for (const el of kept()) el.value = el.name === "cle" ? serverKey : "";
    showCategory();
  });
}

// Every POST form goes out once: a second tap or click would reuse a
// single-use anti-robot token (member form) or hit a stale page (committee).
// The submit button stays enabled: a disabled submitter drops its name=action.
// A page restored from the back/forward cache starts afresh.
const sent = new WeakSet();
for (const form of document.querySelectorAll("form[method=post]")) {
  form.addEventListener("submit", (event) => {
    if (sent.has(form)) event.preventDefault();
    else sent.add(form);
  });
}
window.addEventListener("pageshow", (event) => {
  if (!event.persisted) return;
  for (const form of document.querySelectorAll("form[method=post]")) sent.delete(form);
});

// Committee board: live updates over SSE (spec §4.2). An event carries a
// type and a request id only: the board fetches itself again and swaps its
// list. A changed row keeps a mark until its request is opened.
const CHANGED = "sos-vpdive-changed";

const changedIds = () => {
  try {
    return new Set(JSON.parse(sessionStorage.getItem(CHANGED) || "[]"));
  } catch {
    return new Set();
  }
};

const saveChanged = (ids) => {
  try {
    sessionStorage.setItem(CHANGED, JSON.stringify([...ids]));
  } catch {
    // Storage unavailable: marks only last until the next refresh.
  }
};

const markRows = () => {
  const ids = changedIds();
  for (const row of document.querySelectorAll("[data-ticket]")) {
    row.toggleAttribute("data-changed", ids.has(row.dataset.ticket));
  }
};

// Opens the event stream and calls onId(id) for each event of the given types.
const subscribe = (types, onId) => {
  const source = new EventSource("/evenements");
  for (const type of types) {
    source.addEventListener(type, (event) => onId(String(JSON.parse(event.data).id)));
  }
  return source;
};

// At most one fetch in flight: events arriving meanwhile share one more
// fetch afterwards.
let refreshing = false;
let pending = false;
const refreshBoard = async () => {
  if (refreshing) {
    pending = true;
    return;
  }
  refreshing = true;
  try {
    const response = await fetch(location.href, { credentials: "same-origin" });
    if (!response.ok) return;
    const page = new DOMParser().parseFromString(await response.text(), "text/html");
    const fresh = page.querySelector("[data-board]");
    const current = document.querySelector("[data-board]");
    if (fresh && current) current.replaceWith(fresh);
    markRows();
  } catch {
    // Network down: EventSource reconnects, then the board catches up.
  } finally {
    refreshing = false;
    if (pending) {
      pending = false;
      refreshBoard();
    }
  }
};

if (document.querySelector("[data-board]")) {
  markRows();
  const source = subscribe(["created", "changed", "replied", "deleted"], (id) => {
    const ids = changedIds();
    ids.add(id);
    saveChanged(ids);
    refreshBoard();
  });
  let connected = false;
  source.addEventListener("open", () => {
    if (connected) refreshBoard(); // back after a cut: catch up on everything
    connected = true;
  });
}

// Request page: nothing reloads by itself, so a reply being typed is never
// lost. A banner offers to reload when the request changed elsewhere.
const ticketPage = document.querySelector("[data-ticket-page]");
if (ticketPage) {
  const id = ticketPage.dataset.ticketPage;
  const ids = changedIds();
  if (ids.delete(id)) saveChanged(ids);
  const banner = document.querySelector("[data-changed-banner]");
  subscribe(["changed", "replied", "deleted"], (changed) => {
    if (banner && changed === id) banner.hidden = false;
  });
}

// Request page: copy buttons, shown only when this script runs.
for (const button of document.querySelectorAll("[data-copy]")) {
  button.hidden = false;
  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(button.dataset.copy);
      button.textContent = "Copié";
    } catch {
      button.textContent = "Copie impossible";
    }
  });
}

// Installable app (spec §9.6): register the service worker. A new version
// waits until the member or resolver taps « Recharger », so nothing being
// typed is ever lost to a forced reload.
if ("serviceWorker" in navigator) {
  const banner = document.querySelector("[data-update]");
  let reloading = false;
  const offer = (worker) => {
    if (!banner) return;
    banner.hidden = false;
    banner.querySelector("[data-update-reload]").addEventListener(
      "click",
      () => {
        reloading = true;
        worker.postMessage("skip-waiting");
      },
      { once: true },
    );
  };
  navigator.serviceWorker.addEventListener("controllerchange", () => {
    if (reloading) location.reload();
  });
  navigator.serviceWorker
    .register("/sw.js")
    .then((registration) => {
      if (registration.waiting && navigator.serviceWorker.controller) offer(registration.waiting);
      registration.addEventListener("updatefound", () => {
        const worker = registration.installing;
        worker?.addEventListener("statechange", () => {
          if (worker.state === "installed" && navigator.serviceWorker.controller) offer(worker);
        });
      });
    })
    .catch(() => {
      // No service worker (private browsing, for instance): the site works without it.
    });
}

// Offline page: try the page again.
for (const button of document.querySelectorAll("[data-reload]")) {
  button.addEventListener("click", () => location.reload());
}

// Committee notifications page (spec §9.6): push on or off for this device.
// pushManager.subscribe comes first in the tap handler: Safari asks for the
// permission only from a direct tap.
const pushBox = document.querySelector("[data-push]");
if (pushBox) {
  const show = (state) => {
    for (const el of pushBox.querySelectorAll("[data-push-state]")) el.hidden = el.dataset.pushState !== state;
  };
  const failed = (on) => {
    pushBox.querySelector("[data-push-error]").hidden = !on;
  };
  const csrf = document.querySelector('input[name="csrf"]')?.value ?? "";
  const post = async (path, fields) => {
    const response = await fetch(path, { method: "POST", body: new URLSearchParams({ csrf, ...fields }) });
    if (!response.ok) throw new Error(`${path}: ${response.status}`);
  };
  const base64 = pushBox.dataset.vapidKey.replaceAll("-", "+").replaceAll("_", "/");
  const serverKey = Uint8Array.from(atob(base64), (c) => c.charCodeAt(0));
  if (!("serviceWorker" in navigator && "PushManager" in window && "Notification" in window)) {
    show(navigator.standalone === false ? "install" : "unsupported");
  } else if (Notification.permission === "denied") {
    show("denied");
  } else {
    show(pushBox.dataset.subscribed === "true" ? "on" : "off");
    navigator.serviceWorker.ready.then((registration) => {
      pushBox.querySelector("[data-push-on]").addEventListener("click", async () => {
        failed(false);
        try {
          const subscription = await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: serverKey });
          const { p256dh, auth } = subscription.toJSON().keys;
          await post("/push/abonnement", { endpoint: subscription.endpoint, p256dh, auth });
          show("on");
        } catch {
          if (Notification.permission === "denied") show("denied");
          else failed(true);
        }
      });
      pushBox.querySelector("[data-push-off]").addEventListener("click", async () => {
        failed(false);
        try {
          await post("/push/desabonnement", {});
          await (await registration.pushManager.getSubscription())?.unsubscribe();
          show("off");
        } catch {
          failed(true);
        }
      });
    });
  }
}
