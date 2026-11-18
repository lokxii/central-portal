// @ts-check
// Offline shell for Central Portal.

const CACHE = "portal-v3";

/** @type {string[]} */
const PRECACHE = [
  "/",
  "/index.html",
  "/app.js",
  "/style.css",
  "/manifest.webmanifest",
  "/icons/icon-192.png",
  "/icons/icon-512.png",
  "/icons/icon-maskable-512.png",
  "/icons/apple-touch-icon.png",
];

/** @type {any} */
const worker = self;

/** @returns {Response} */
function offline() {
  return new Response("Offline", {
    status: 503,
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
}

worker.addEventListener("install", (/** @type {any} */ event) => {
  event.waitUntil(
    caches.open(CACHE)
      .then((cache) => cache.addAll(PRECACHE.map((url) => new Request(url, { cache: "reload" }))))
      .then(() => worker.skipWaiting())
  );
});

worker.addEventListener("activate", (/** @type {any} */ event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(
        keys
          .filter((key) => key.startsWith("portal-") && key !== CACHE)
          .map((key) => caches.delete(key))
      ))
      .then(() => worker.clients.claim())
  );
});

worker.addEventListener("fetch", (/** @type {any} */ event) => {
  /** @type {Request} */
  const request = event.request;
  if (request.method !== "GET") return;

  const url = new URL(request.url);
  if (url.origin !== worker.location.origin) return;
  if (url.pathname.startsWith("/api/") || url.pathname === "/ca.crt") return;

  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request).catch(() => caches.match("/index.html").then((cached) => cached || offline()))
    );
    return;
  }

  if (url.pathname === "/app.js") {
    // Network-first so a fresh index.html cannot pair with a stale module.
    event.respondWith(
      fetch(request).then((response) => {
        if (response.ok) {
          const copy = response.clone();
          caches.open(CACHE).then((cache) => cache.put(request, copy)).catch(() => {});
        }
        return response;
      }).catch(() => caches.match(request).then((cached) => cached || offline()))
    );
    return;
  }

  event.respondWith(
    caches.match(request).then((cached) => {
      const network = fetch(request).then((response) => {
        if (response.ok) {
          const copy = response.clone();
          caches.open(CACHE).then((cache) => cache.put(request, copy)).catch(() => {});
        }
        return response;
      }).catch(() => cached || offline());
      return cached || network;
    })
  );
});
