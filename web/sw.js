/*
 * {{.AppName}} service worker — installability + offline shell ONLY.
 *
 * Safety contract (must never break web / Android / iPhone):
 *   1. Only same-origin GET requests for the STATIC SHELL are intercepted.
 *      Everything else — every API endpoint, uploaded files, the WebSocket,
 *      Google GSI, any POST/PUT/etc. — is left completely untouched and goes
 *      straight to the network as if no service worker existed.
 *
 *      This used to intercept ANY same-origin GET: the header promised the
 *      shell while the code took everything, so responses from /history,
 *      /search, /files/ and /api/profile ended up in Cache Storage. That is,
 *      decrypted conversations and attachments settled on the browser's disk —
 *      where nobody expects them and where neither signing out nor revoking a
 *      token erases them. Now a path must EXPLICITLY be on the allowlist
 *      (isShell): a new API endpoint cannot slip into the cache silently.
 *   2. Network-first for the static shell, so an online client ALWAYS receives
 *      the freshest HTML/JS/CSS. The cache is only a fallback when offline.
 *   3. Old caches are purged on activate; the new worker claims clients
 *      immediately, so a deploy can never get "stuck" behind a stale worker.
 */
const VERSION = '{{.AppSlug}}-v6';
const SHELL = [
  '/',
  '/index.html',
  '/manifest.json',
  '/LOGO_DARK.svg',
  '/css/app.css',
  // The font files themselves are not listed: there are forty of them, and the
  // browser takes only the subsets for the characters actually used
  // (unicode-range). Caching all of them means pulling over a megabyte for
  // alphabets that are not on screen. Downloaded ones land in the cache the
  // ordinary way (network-first below).
  '/css/fonts.css',
  '/assets/icons/icon-192.png',
  '/assets/icons/icon-512.png',
  '/assets/icons/apple-touch-icon.png',
  '/js/main.js',
  '/js/a11y.js',
  '/js/attachpanel.js',
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
  '/js/theme.js',
  '/js/richtext.js',
  '/js/lightbox.js',
  '/js/backstack.js',
  // Below are the modules that were missing from the list. Without events.js
  // not a single button works (all click routing goes through data-act), and
  // without bootstrap.js the configuration is not applied. That is, an offline
  // launch from the home screen opened a dead app until the network came back.
  '/js/bootstrap.js',
  '/js/events.js',
  '/js/profiles.js',
  '/js/search.js',
  '/js/chatsearch.js',
  '/js/chatmenu.js',
  '/js/groups.js',
  '/js/settings.js',
  '/js/linkpreview.js',
  '/js/nettest.js',
  '/js/voice.js',
  '/js/sso.js',
  '/js/twofa.js',
  '/js/appconfig.js',
];

// isShell — the allowlist of what may be put in the cache at all.
//
// An allowlist and not a denylist: with a denylist any NEW API endpoint ends up
// cached by default, and it is remembered only once the data has already
// leaked to disk. Here it is the other way round — an unknown path is not cached.
function isShell(url) {
  const p = url.pathname;
  return p === '/' ||
    p === '/index.html' ||
    p === '/manifest.json' ||
    p === '/LOGO_DARK.svg' ||
    p.startsWith('/js/') ||
    p.startsWith('/css/') ||
    p.startsWith('/fonts/') ||
    p.startsWith('/assets/');
}

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
  // A call push behaves like a call: it does not collapse on its own
  // (requireInteraction) and vibrates like a ringtone. Its TTL is 45s on the
  // server, so stale "calls" never reach here.
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

  // (2) Navigations are always intercepted — the offline shell exists for
  //     them: a cold start from the home screen with no network must open the
  //     app, not the browser error page.
  const isNavigation = req.mode === 'navigate';

  // (3) Anything that is neither shell nor navigation we do NOT touch at all.
  //     This is not only about privacy: the request goes to the network
  //     directly, without the respondWith wrapper, so the API is exactly as
  //     fast as it would be without a service worker.
  if (!isNavigation && !isShell(url)) return;

  // (4) Network-first: an online client ALWAYS gets fresh HTML/JS/CSS, and the
  //     cache stays a fallback for when there is no network.
  event.respondWith(
    fetch(req)
      .then((res) => {
        // Only the shell goes into the cache. A navigation response is
        // index.html rendered by the server, and that is shell too; but we
        // store it under the key '/index.html' rather than under the request
        // address, otherwise the cache would collect an entry per path
        // (/, /?sso=…, and so on).
        if (res && res.status === 200 && res.type === 'basic') {
          if (isShell(url)) {
            const copy = res.clone();
            caches.open(VERSION).then((cache) => cache.put(req, copy));
          } else if (isNavigation) {
            const copy = res.clone();
            caches.open(VERSION).then((cache) => cache.put('/index.html', copy));
          }
        }
        return res;
      })
      .catch(async () => {
        const cached = await caches.match(req);
        if (cached) return cached;
        if (isNavigation) {
          return (await caches.match('/index.html')) || (await caches.match('/'));
        }
        return Response.error();
      })
  );
});
