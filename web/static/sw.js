// @ts-check
// Offline shell for Central Portal.

const CACHE = "portal-v2";

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

worker.addEventListener("install", (/** @type {any} */ event) => {
  event.waitUntil(
    caches.open(CACHE)
      .then((cache) => cache.addAll(PRECACHE))
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
      fetch(request).catch(() => caches.match("/index.html").then(
        (cached) => cached || new Response("Offline", {
          status: 503,
          headers: { "Content-Type": "text/plain; charset=utf-8" },
        })
      ))
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
      }).catch(() => cached || new Response("Offline", { status: 503 }));
      return cached || network;
    })
  );
});
