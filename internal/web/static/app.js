// sos-vpdive: the only script of both sites (spec §9.1). It enhances pages
// that work without it; only the anti-robot check needs JavaScript.
"use strict";

// Member form: show the fields of the chosen category only, and require
// theirs only. Hidden fieldsets stay enabled, so that a browser agent (WebMCP)
// sees every field and fills a category's fields in one call.
(() => {
  const select = document.querySelector("[data-category-select]");
  if (!select) return;
  const fieldsets = document.querySelectorAll("fieldset[data-category]");
  const sync = () => {
    for (const fieldset of fieldsets) {
      const chosen = fieldset.dataset.category === select.value;
      fieldset.hidden = !chosen;
      fieldset.disabled = false;
      for (const el of fieldset.querySelectorAll("[data-required]")) el.required = chosen;
    }
  };
  select.addEventListener("change", sync);
  // Native validation runs between the click and the submit event: hidden
  // fieldsets are disabled before it, so that a value left in another category
  // neither blocks sending nor leaves. Enter in a field clicks this button too.
  select.form.querySelector("[type=submit]").addEventListener("click", () => {
    for (const fieldset of fieldsets) fieldset.disabled = fieldset.hidden;
    setTimeout(() => {
      for (const fieldset of fieldsets) fieldset.disabled = false;
    });
  });
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
const draft = readDraft();
// Writing the draft back as it is tells whether storage works at all.
if (draftForm && draftNote && writeDraft(draft)) {
  const serverKey = draftForm.elements.cle.value;
  const kept = () =>
    [...draftForm.elements].filter((el) => el.name && !NOT_KEPT.has(el.name) && !["file", "submit", "button"].includes(el.type));
  const showCategory = () => draftForm.querySelector("[data-category-select]")?.dispatchEvent(new Event("change"));
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

// Committee board: a changed filter applies at once (spec §4.2 as amended);
// « Filtrer » stays for a browser without this script.
for (const form of document.querySelectorAll("form[data-autosubmit]")) {
  form.addEventListener("change", () => form.requestSubmit());
}

// Committee pages: live updates over SSE (spec §4.2). An event carries a
// type, a request id, and whether this resolver made the change: the board
// fetches itself again and swaps its list, the request page offers to
// reload, and desktop pages show a toast. A changed row keeps a mark until
// its request is opened.
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

// Toasts (desktop only, spec §4.2 as amended): a fragment fetched under the
// session, so the stream itself never carries a name. Three at most; each
// closes after ten seconds, later while the pointer or the focus is on it.
const toasts = document.querySelector("[data-toasts]");
const wide = window.matchMedia("(min-width: 64rem)");
const TOAST_MS = 10000;
const showToast = async (type, id) => {
  try {
    const response = await fetch(`/demandes/${id}/toast?type=${type}`, { credentials: "same-origin" });
    if (!response.ok) return;
    const toast = new DOMParser().parseFromString(await response.text(), "text/html").querySelector("[data-toast]");
    if (!toast) return;
    toasts.append(toast);
    while (toasts.children.length > 3) toasts.firstElementChild.remove();
    const expire = () => {
      if (toast.matches(":hover, :focus-within")) setTimeout(expire, TOAST_MS);
      else toast.remove();
    };
    setTimeout(expire, TOAST_MS);
    toast.querySelector("[data-toast-close]").addEventListener("click", () => toast.remove());
  } catch {
    // Network down: no toast; the board catches up on its own.
  }
};

const board = document.querySelector("[data-board]");
const ticketPage = document.querySelector("[data-ticket-page]");
const shown = ticketPage?.dataset.ticketPage;
// One stream per page, opened on first use. Phones open it on the board and
// the request page only, as before: push notifications tell them the rest.
let live = null;
const stream = () => (live ??= new EventSource("/evenements"));
const onChange = (types, handler) => {
  for (const type of types) {
    stream().addEventListener(type, (event) => {
      const data = JSON.parse(event.data);
      handler(type, String(data.id), data.self === true);
    });
  }
};

if (board) {
  markRows();
  onChange(["created", "changed", "replied", "deleted"], (type, id) => {
    const ids = changedIds();
    ids.add(id);
    saveChanged(ids);
    refreshBoard();
  });
  let connected = false;
  stream().addEventListener("open", () => {
    if (connected) refreshBoard(); // back after a cut: catch up on everything
    connected = true;
  });
}

// Request page: nothing reloads by itself, so a reply being typed is never
// lost. A banner offers to reload when the request changed elsewhere.
if (ticketPage) {
  const ids = changedIds();
  if (ids.delete(shown)) saveChanged(ids);
  const banner = document.querySelector("[data-changed-banner]");
  onChange(["changed", "replied", "deleted"], (type, id) => {
    if (banner && id === shown) banner.hidden = false;
  });
}

// Toasts start with a wide window, or when a narrow one grows wide.
let toasting = false;
const startToasts = () => {
  if (toasting || toasts === null || !wide.matches) return;
  toasting = true;
  onChange(["created", "changed", "replied"], (type, id, self) => {
    if (!self && id !== shown && wide.matches) showToast(type, id);
  });
};
startToasts();
wide.addEventListener("change", startToasts);

// A tap on a notification while the app is open (service worker): go to the
// request, unless something here is not sent yet; then a banner offers it.
// Unsent: a field, a box or a choice changed by hand, or a reply or a note
// the server wrote back after a refused action, in its field or in « Ton
// texte » ([data-unsent]) when the request no longer takes it.
const unsent = () =>
  document.querySelector("[data-unsent]") !== null ||
  [...document.querySelectorAll("form[method=post] :is(textarea, input, select)")].some((el) => {
    if (el.type === "file") return el.files.length > 0;
    if (el.type === "checkbox" || el.type === "radio") return el.checked !== el.defaultChecked;
    if (el.localName === "select") return el.selectedIndex !== Math.max(0, [...el.options].findIndex((o) => o.defaultSelected));
    if (el.localName === "textarea" && el.value.trim() !== "") return true;
    return el.type !== "hidden" && el.value !== el.defaultValue;
  });
navigator.serviceWorker?.addEventListener("message", (event) => {
  const url = typeof event.data?.open === "string" ? new URL(event.data.open, location.href) : null;
  if (!url || url.origin !== location.origin) return;
  if (!unsent()) {
    location.assign(url);
    return;
  }
  const banner = document.querySelector("[data-open-banner]");
  if (!banner) return;
  banner.querySelector("[data-open-link]").href = url.href;
  banner.hidden = false;
});

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
// typed is ever lost to a forced reload. The registration rejects when the
// browser refuses service workers (private browsing, for instance).
const registered = "serviceWorker" in navigator ? navigator.serviceWorker.register("/sw.js") : null;
if (registered) {
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
  registered
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

// Committee assistant (design 2026-10-08): sends a question and reads the
// answer as it streams, one JSON event per line. The server renders every
// piece of HTML (answer, dossier, sources) and this script only places it;
// whatever comes from the model or from an import (step labels, error
// texts) is set as text.
const assistantBox = (box) => {
  const form = box.querySelector("[data-assistant-form]");
  const field = form.elements.text;
  const thread = box.querySelector("[data-assistant-thread]");
  const shape = box.querySelector("template[data-assistant-new]");
  const send = form.querySelector("[data-assistant-send]");
  const sticky = box.querySelector("[data-assistant-sticky]"); // the composer of /assistant, over the page
  let running = null; // AbortController of the answer in flight

  const busy = (on) => {
    send.querySelector("[data-label-send]").hidden = on;
    send.querySelector("[data-label-stop]").hidden = !on;
    // On the live region itself: a screen reader waits for the end of the answer.
    thread.setAttribute("aria-busy", on ? "true" : "false");
  };

  // Where the visible thread ends: above the sticky composer of /assistant,
  // or at the foot of the box that scrolls in the request panel.
  const floor = () => (sticky ? sticky.getBoundingClientRect().top : thread.parentElement.getBoundingClientRect().bottom);
  // Brings the foot of an exchange above the composer, which scrollIntoView
  // ignores: its height (and the phone tab bar's) becomes the scroll margin.
  const toEnd = (node) => {
    if (sticky) node.style.scrollMarginBottom = `${window.innerHeight - floor() + 16}px`;
    node.scrollIntoView({ block: "end" });
  };
  const atEnd = (node) => node.getBoundingClientRect().bottom <= floor() + 40;
  const setRemaining = (text) => {
    if (text) for (const r of document.querySelectorAll("[data-assistant-remaining]")) r.textContent = text;
  };

  const ask = async (question) => {
    if (running) return;
    const node = shape.content.firstElementChild.cloneNode(true);
    const asked = node.querySelector("[data-question]");
    if (question) asked.textContent = question;
    else asked.hidden = true; // « Analyser »: the start event names the request
    document.querySelector("[data-assistant-empty]")?.remove();
    thread.append(node);
    const stepsBox = node.querySelector("[data-steps-box]");
    const steps = node.querySelector("[data-steps]");
    const summary = node.querySelector("[data-steps-summary]");
    const answer = node.querySelector("[data-answer]");
    const error = node.querySelector("[data-error]");
    let ended = false; // a "done" or an "error" event came
    // Applies a change to the exchange and keeps its foot in view, unless the resolver scrolled away.
    const grow = (change) => {
      const follow = atEnd(node);
      change();
      if (follow) toEnd(node);
    };
    const tally = () => {
      const n = steps.children.length;
      summary.textContent = n === 0 ? "Aucune donnée consultée" : n === 1 ? "1 donnée consultée" : `${n} données consultées`;
    };
    const fail = (message) => {
      ended = true;
      error.textContent = message;
      error.hidden = false;
      if (steps.children.length) {
        tally();
        stepsBox.open = false;
      } else {
        stepsBox.hidden = true;
      }
    };
    const show = (event) => {
      switch (event.type) {
        case "start":
          box.dataset.conversation = event.conversation;
          setRemaining(event.remaining); // counted from now on, even if the answer stops
          if (event.question) {
            asked.textContent = event.question;
            asked.hidden = false;
          }
          if (event.url) history.replaceState(null, "", event.url);
          break;
        case "thinking":
          summary.textContent = "Réflexion…";
          break;
        case "step": {
          const li = document.createElement("li");
          li.textContent = event.label;
          steps.append(li);
          tally();
          break;
        }
        case "answer":
          answer.innerHTML = event.html ?? ""; // none: what streamed is no answer
          break;
        case "dossier":
          for (const d of document.querySelectorAll("[data-assistant-dossier]")) d.innerHTML = event.html;
          for (const d of document.querySelectorAll("[data-assistant-dossier-summary]")) d.textContent = event.summary;
          break;
        case "done":
          ended = true;
          answer.innerHTML = event.html;
          node.querySelector("[data-sources]").innerHTML = event.sources ?? "";
          stepsBox.open = false;
          tally();
          setRemaining(event.remaining);
          for (const label of document.querySelectorAll("[data-assistant-open-label]")) label.textContent = "Voir l'analyse";
          break;
        case "error":
          fail(event.message);
          setRemaining(event.remaining);
          break;
      }
    };
    toEnd(node);
    const body = new URLSearchParams({
      csrf: form.elements.csrf.value,
      text: question,
      conversation: box.dataset.conversation ?? "",
      demande: box.dataset.demande ?? "",
    });
    running = new AbortController();
    busy(true);
    try {
      const response = await fetch(form.action, { method: "POST", body, signal: running.signal });
      if (!response.ok) {
        // Erased (404) or full (410): the next question starts a new conversation.
        if (response.status === 404 || response.status === 410) delete box.dataset.conversation;
        const message = (await response.text()).trim() || "Erreur : réessaie.";
        grow(() => fail(message));
        return;
      }
      const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
      let buffer = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += value;
        let end;
        while ((end = buffer.indexOf("\n")) >= 0) {
          const line = buffer.slice(0, end);
          buffer = buffer.slice(end + 1);
          if (line) {
            const event = JSON.parse(line);
            grow(() => show(event));
          }
        }
      }
      if (!ended) grow(() => fail("Connexion perdue : réessaie."));
    } catch (err) {
      // A drop or a stop after "done" leaves the kept answer as it is.
      if (!ended) grow(() => fail(err.name === "AbortError" ? "Réponse arrêtée." : "Connexion perdue : réessaie."));
    } finally {
      running = null;
      busy(false);
    }
  };

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    if (running) {
      running.abort();
      return;
    }
    const question = field.value.trim();
    if (!question) {
      field.focus();
      return;
    }
    field.value = "";
    field.style.height = "";
    ask(question);
  });
  field.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      if (!running) form.requestSubmit(); // while an answer runs, only the button stops it
    }
  });
  // Grows with its text: CSSOM, allowed by the style-src CSP.
  field.addEventListener("input", () => {
    field.style.height = "auto";
    field.style.height = `${field.scrollHeight}px`;
  });
  box.addEventListener("click", (event) => {
    const starter = event.target.closest("[data-starter]");
    if (starter) {
      field.value = starter.dataset.starter;
      field.focus();
    }
    const pick = event.target.closest("[data-pick]");
    if (pick) ask(pick.dataset.pick);
  });
  // « Nouvelle analyse » (request panel): the analysis starts again, in a new conversation.
  box.querySelector("[data-assistant-restart]")?.addEventListener("click", () => {
    if (running) return;
    for (const ex of thread.querySelectorAll("[data-exchange]")) ex.remove();
    delete box.dataset.conversation;
    ask("");
  });
  return { ask, field, isRunning: () => running !== null };
};

const assistants = new Map();
for (const box of document.querySelectorAll("[data-assistant]")) assistants.set(box, assistantBox(box));

// A draft reply (the model's « brouillon » block): its button copies the text.
document.addEventListener("click", async (event) => {
  const button = event.target.closest("[data-copy-draft]");
  if (!button) return;
  try {
    await navigator.clipboard.writeText(button.closest("[data-draft-reply]").querySelector("pre").textContent.trim());
    button.textContent = "Copié";
  } catch {
    button.textContent = "Copie impossible : sélectionne le texte";
  }
});

// Request page: « Analyser » opens the panel beside the request (a sheet on
// a phone) and starts the analysis unless an answer is there already;
// closing returns to the same place.
const panel = document.querySelector("aside[data-assistant]");
const opener = document.querySelector("[data-assistant-open]");
if (panel && opener) {
  let scroll = 0;
  const heading = panel.querySelector("h2");
  const openPanel = (on) => {
    if (on) scroll = window.scrollY;
    panel.hidden = !on;
    opener.setAttribute("aria-expanded", String(on));
    ticketPage?.toggleAttribute("data-panel-open", on);
    if (on) {
      heading.focus({ preventScroll: true }); // on a phone the sheet covers the opener: focus goes into it
    } else {
      opener.focus({ preventScroll: true });
      if (!wide.matches) window.scrollTo(0, scroll);
    }
  };
  opener.addEventListener("click", () => {
    openPanel(true);
    const box = assistants.get(panel);
    const exchanges = [...panel.querySelectorAll("[data-exchange]")];
    // An answer counts when it ended without an error; partial text before an error or a stop does not.
    const answered = (ex) => ex.querySelector("[data-error]").hidden && ex.querySelector("[data-answer]").hasChildNodes();
    if (box.isRunning() || exchanges.some(answered)) {
      if (wide.matches) box.field.focus(); // a phone's keyboard would cover the answer
      return;
    }
    // Never run or failed: start again, without the old error or partial answer above.
    for (const ex of exchanges) ex.remove();
    box.ask("");
  });
  for (const b of panel.querySelectorAll("[data-assistant-close]")) b.addEventListener("click", () => openPanel(false));
  panel.addEventListener("keydown", (event) => {
    if (event.key === "Escape") openPanel(false);
  });
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
  if (!(registered && "PushManager" in window && "Notification" in window)) {
    show(navigator.standalone === false ? "install" : "unsupported");
  } else if (Notification.permission === "denied") {
    show("denied");
  } else {
    show(pushBox.dataset.subscribed === "true" ? "on" : "off");
    // ready never settles without a registered worker: wait for the
    // registration first, and say so when the browser refused it.
    registered.then(() => navigator.serviceWorker.ready).then((registration) => {
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
    }, () => show("unsupported"));
  }
}

// Page views (spec §9.10): Umami's script is requested only without Do Not
// Track, and a page is reported by its route template, never by its real
// address, the page it came from or its title, which may hold a reference.
(() => {
  const page = document.body.dataset;
  if (!page.umamiSrc || navigator.doNotTrack === "1" || window.doNotTrack === "1") return;
  const script = document.createElement("script");
  script.async = true;
  script.src = page.umamiSrc;
  script.dataset.websiteId = page.umamiWebsite;
  script.dataset.autoTrack = "false";
  script.dataset.doNotTrack = "true";
  script.addEventListener("load", () =>
    window.umami?.track((props) => ({ ...props, url: page.umamiPath, referrer: "", title: "" })),
  );
  document.head.append(script);
})();
