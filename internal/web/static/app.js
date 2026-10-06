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
