/*
 * {{.AppName}} service worker — installability + offline shell ONLY.
 *
 * Safety contract (must never break web / Android / iPhone):
 *   1. Only same-origin GET requests are ever intercepted. Everything else —
 *      cross-origin requests, Google sign-in, any POST/PUT and so on — is
 *      left untouched and goes straight to the network as if no service
 *      worker existed.
 *   2. Network-first for the static shell, so an online client ALWAYS receives
 *      the freshest HTML/JS/CSS. The cache is only a fallback when offline.
 *   3. Old caches are purged on activate; the new worker claims clients
 *      immediately, so a deploy can never get "stuck" behind a stale worker.
 */
const VERSION = '{{.AppSlug}}-v1';
const SHELL = [
  '/',
  '/index.html',
  '/manifest.json',
  '/LOGO_DARK.svg',
  '/css/app.css',
  // The font files themselves are not listed: there are forty of them and the
  // browser fetches only the subsets it needs for the characters on screen.
  // Caching all of them would pull over a megabyte for alphabets nobody is
  // displaying. Whatever is downloaded lands in the cache the usual way.
  '/css/fonts.css',
  '/assets/icons/icon-192.png',
  '/assets/icons/icon-512.png',
  '/assets/icons/apple-touch-icon.png',
  '/js/main.js',
  '/js/state.js',
  '/js/storage.js',
  '/js/session.js',
  '/js/auth.js',
  '/js/transport.js',
  '/js/history.js',
  '/js/helpers.js',
  '/js/ui.js',
  '/js/upload.js',
  '/js/gestures.js',
  '/js/menu.js',
  '/js/calls.js',
  '/js/push.js',
];

self.addEventListener('install', (event) => {
  self.skipWaiting();
  event.waitUntil(
    caches.open(VERSION).then((cache) =>
      // allSettled: a single missing/renamed asset must never fail the install.
      Promise.allSettled(SHELL.map((u) => cache.add(u)))
    )
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== VERSION).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// ── Web Push ────────────────────────────────────────────────
// iOS requires every push to result in a visible notification, otherwise the
// subscription can be revoked. We always show one.
self.addEventListener('push', (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch { data = { body: event.data && event.data.text() }; }
  const title = data.title || '{{.AppName}}';
  // A call push behaves like a call: it does not dismiss itself
  // (requireInteraction) and vibrates like a ringtone. Its TTL is 45 s on the
  // server, so stale "calls" never reach this point.
  const isCall = data.kind === 'call' || (data.tag || '').startsWith('call-');
  const options = {
    body: data.body || 'New message',
    icon: '/assets/icons/icon-192.png',
    badge: '/assets/icons/icon-192.png',
    tag: data.tag || 'hexeris-message',
    renotify: true,
    requireInteraction: isCall,
    vibrate: isCall ? [300, 100, 300, 100, 300] : undefined,
    // Accept/Decline straight from the notification (Android/desktop). iOS
    // ignores actions but still shows the notification + deep-link on tap.
    actions: isCall ? [
      { action: 'accept', title: '📞 Accept' },
      { action: 'decline', title: '✕ Decline' },
    ] : undefined,
    data: { url: data.url || '/', from: data.from || '', kind: data.kind || '' },
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', (event) => {
  event.notification.close();
  const d = event.notification.data || {};
  let target = d.url || '/';
  // Encode the chosen call action into the deep-link so the app can act on it
  // the moment it (re)connects: accept → auto-answer, decline → auto-reject.
  if (d.kind === 'call' && d.from) {
    const act = event.action === 'decline' ? 'decline' : 'accept';
    target = '/?call=' + encodeURIComponent(d.from) + '&callaction=' + act;
  }
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((clients) => {
      for (const c of clients) {
        if ('focus' in c) { if (c.navigate) c.navigate(target); return c.focus(); }
      }
      if (self.clients.openWindow) return self.clients.openWindow(target);
    })
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;

  // (1) Never touch non-GET or cross-origin (API, WSS, Google) requests.
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // (2) Network-first: fresh when online, cached shell when offline.
  event.respondWith(
    fetch(req)
      .then((res) => {
        // Only cache full, same-origin successful responses.
        if (res && res.status === 200 && res.type === 'basic') {
          const copy = res.clone();
          caches.open(VERSION).then((cache) => cache.put(req, copy));
        }
        return res;
      })
      .catch(async () => {
        const cached = await caches.match(req);
        if (cached) return cached;
        // For navigations, fall back to the app shell so a cold offline
        // launch from the home screen still renders.
        if (req.mode === 'navigate') {
          return (await caches.match('/index.html')) || (await caches.match('/'));
        }
        return Response.error();
      })
  );
});
