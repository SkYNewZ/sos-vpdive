// sos-vpdive service worker, {{if .Admin}}committee{{else}}members{{end}} site (spec §9.6). It
// caches a closed list, the static files and the offline page, nothing else:
// pages and screenshots hold personal data and always come from the network.
"use strict";

const CACHE = "sos-{{.Version}}";
const PRECACHE = {{.Precache}};
const OFFLINE = "{{.Offline}}";

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

// A tap brings up the request: the window already on it, else an open
// window, asked to go there (app.js goes unless a reply is being typed), else
// a new one. An installed app on iOS has a single window, which openWindow
// only wakes up without loading the request. The first page after install is
// not controlled yet: it counts as an open window too. A screenshot opened in
// its own tab runs no app.js: it does not.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL(event.notification.data.url, location.origin).href;
  event.waitUntil(
    (async () => {
      const windows = (await clients.matchAll({ type: "window", includeUncontrolled: true })).filter(
        (client) => !new URL(client.url).pathname.includes("/captures/"),
      );
      const there = windows.find((client) => client.url === url);
      if (there) return there.focus();
      if (windows.length > 0) {
        windows[0].postMessage({ open: url });
        return windows[0].focus();
      }
      return clients.openWindow(url);
    })(),
  );
});
{{end}}
