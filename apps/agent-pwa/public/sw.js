/* Meridian Agent PWA service worker: cache-first app shell + outbox sync. */
const CACHE = 'meridian-agent-v1';

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE).then((c) => c.addAll(['./', './index.html', './manifest.webmanifest', './icon.svg']))
  );
  self.skipWaiting();
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
  );
  self.clients.claim();
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  // never cache API calls
  if (e.request.method !== 'GET' || url.pathname.startsWith('/v1/')) return;
  e.respondWith(
    caches.match(e.request, { ignoreSearch: true }).then((hit) => {
      return (
        hit ||
        fetch(e.request).then((resp) => {
          if (resp.ok && url.origin === location.origin) {
            const copy = resp.clone();
            caches.open(CACHE).then((c) => c.put(e.request, copy));
          }
          return resp;
        }).catch(() => caches.match('./index.html'))
      );
    })
  );
});

// Background Sync: flush the IndexedDB outbox when connectivity returns.
self.addEventListener('sync', (e) => {
  if (e.tag === 'outbox-sync') {
    e.waitUntil(self.clients.matchAll().then((clients) => {
      clients.forEach((c) => c.postMessage({ type: 'OUTBOX_SYNC_REQUESTED' }));
    }));
  }
});
