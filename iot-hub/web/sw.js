// Service worker: makes the dashboard installable and lets the shell open
// without a connection. Only the app shell is cached, network first, so a
// new hub version is picked up on the next online load. API responses are
// never cached: no plant data or authenticated content is stored on the
// device, and an offline dashboard never shows old values as if live.
const CACHE = 'iothub-shell-v4';
const SHELL = ['/', '/index.html', '/app.js', '/core.js', '/tiles.js', '/style.css', '/manifest.webmanifest', '/icon.svg', '/icon-192.png',
  ...['overview', 'dashboard', 'alarms', 'sensors', 'devices', 'commands', 'jobs', 'notifications', 'grafana', 'tenants', 'system'].map((p) => `/pages/${p}.js`)];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys()
    .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== self.location.origin || url.pathname.startsWith('/api/')) return;
  e.respondWith(fetch(e.request).then((res) => {
    if (res.ok && res.type === 'basic') {
      const copy = res.clone();
      e.waitUntil(caches.open(CACHE).then((c) => c.put(e.request, copy)));
    }
    return res;
  }).catch(async () => (await caches.match(e.request, { ignoreSearch: true }))
    || (e.request.mode === 'navigate' && await caches.match('/index.html'))
    || Response.error()));
});
