// sos-vpdive service worker, {{if .Admin}}committee{{else}}members{{end}} site (spec §9.6). It
// caches a closed list, the static files and the offline page, nothing else:
// pages and screenshots hold personal data and always come from the network.
"use strict";

const CACHE = "sos-{{.Version}}";
const PRECACHE = {{.Precache}};
const OFFLINE = "/hors-ligne";

// No skipWaiting here: the new version waits until the page offers to reload.
self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE).then((cache) => cache.addAll(PRECACHE.map((url) => new Request(url, { cache: "reload" })))),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      for (const name of await caches.keys()) {
        if (name.startsWith("sos-") && name !== CACHE) await caches.delete(name);
      }
      if (self.registration.navigationPreload) await self.registration.navigationPreload.enable();
    })(),
  );
});

self.addEventListener("message", (event) => {
  if (event.data === "skip-waiting") self.skipWaiting();
});

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.mode === "navigate") {
    event.respondWith(
      (async () => {
        try {
          return (await event.preloadResponse) || (await fetch(request));
        } catch {
          return (await caches.match(OFFLINE)) || Response.error();
        }
      })(),
    );
    return;
  }
  const url = new URL(request.url);
  if (request.method === "GET" && url.origin === location.origin && PRECACHE.includes(url.pathname + url.search)) {
    event.respondWith(caches.match(request).then((cached) => cached || fetch(request)));
  }
});
{{if .Admin}}
// Committee alerts. Every push shows a notification, even an unreadable one:
// Safari withdraws the permission of a site that does not.
self.addEventListener("push", (event) => {
  let alert = {};
  try {
    alert = event.data.json();
  } catch {
    // Unreadable: the defaults below still tell a resolver to look.
  }
  event.waitUntil(
    self.registration.showNotification(alert.title || "SOS CPP Comité", {
      body: alert.body || "",
      icon: "{{.Icon}}",
      data: { url: alert.url || "/" },
    }),
  );
});

// A tap brings up the request: the window already on it, else a new one.
// Another open window is never steered away: it may hold a reply being typed.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(event.notification.data.url, location.origin).href;
  event.waitUntil(
    (async () => {
      for (const client of await clients.matchAll({ type: "window" })) {
        if (client.url === url) return client.focus();
      }
      return clients.openWindow(url);
    })(),
  );
});
{{end}}
