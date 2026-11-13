// clanka's service worker: precache the app shell, keep API reads
// network-first, and answer an unreachable Pi with the cached shell (or a
// plain offline page) instead of a browser error. The build replaces the two
// placeholders with the asset list and a content-derived version.
const VERSION = "__VERSION__";
const PRECACHE = __PRECACHE__;
const CACHE = "clanka-" + VERSION;
const OFFLINE_URL = "/offline.html";

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then((cache) => cache.addAll(PRECACHE))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((key) => key !== CACHE).map((key) => caches.delete(key))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET") return;
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  if (request.mode === "navigate") {
    event.respondWith(navigate(request));
    return;
  }
  if (url.pathname.startsWith("/api/")) {
    // Reads stay network-first so the page is never stale; a failed read says
    // so in the app's own words.
    event.respondWith(
      fetch(request).catch(
        () =>
          new Response(JSON.stringify({ Error: "The assistant is unreachable." }), {
            status: 503,
            headers: { "Content-Type": "application/json; charset=utf-8" },
          }),
      ),
    );
    return;
  }
  event.respondWith(cacheFirst(request));
});

// The document is fetched from the Pi so a new build is picked up, and the
// last good copy opens when it is not reachable.
async function navigate(request) {
  try {
    const response = await fetch(request);
    if (response.ok) {
      const cache = await caches.open(CACHE);
      cache.put("/index.html", response.clone());
    }
    return response;
  } catch {
    return (await caches.match("/index.html")) || (await caches.match(OFFLINE_URL)) || Response.error();
  }
}

async function cacheFirst(request) {
  const hit = await caches.match(request);
  if (hit) return hit;
  const response = await fetch(request);
  if (response.ok) {
    const cache = await caches.open(CACHE);
    cache.put(request, response.clone());
  }
  return response;
}
