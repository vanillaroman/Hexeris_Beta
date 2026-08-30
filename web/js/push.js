// Hexeris — Web Push (iOS Home-Screen PWA, Android, desktop).
//
// iOS note: Web Push works ONLY when the site is installed to the Home Screen
// (standalone) on iOS 16.4+, and Notification.requestPermission() must be
// triggered by a user gesture (the "Notifications" button click).
//
// The subscription is POSTed to /api/push/subscribe (same-origin). During local
// development that endpoint is the node dev server; in production it will be the
// Go backend. Nothing here talks to the messaging backend (SERVER) directly.

function _urlBase64ToUint8Array(base64String) {
  const padding = '='.repeat((4 - (base64String.length % 4)) % 4);
  const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const arr = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) arr[i] = raw.charCodeAt(i);
  return arr;
}

// pushOptedOut records that the user switched notifications off themselves.
//
// It has to be stored: unsubscribing removes the subscription, but JS cannot
// revoke the browser permission, which stays "granted". Deriving the state
// from permission plus subscription alone made ensurePushSubscribed see
// "allowed but not subscribed" on the next connect and quietly subscribe
// again — the user turned notifications off, reloaded, and they were back.
//
// The key is prefixed so it is cleared on sign-out, meaning the next owner of
// the device gets the default behaviour rather than someone else's opt-out.
const PUSH_OPTOUT_KEY = 'hc_push_off';

function pushOptedOut() {
  try { return localStorage.getItem(PUSH_OPTOUT_KEY) === '1'; } catch { return false; }
}

function setPushOptOut(off) {
  try {
    if (off) localStorage.setItem(PUSH_OPTOUT_KEY, '1');
    else localStorage.removeItem(PUSH_OPTOUT_KEY);
  } catch {}
}

function pushSupported() {
  return ('serviceWorker' in navigator) && ('PushManager' in window) && ('Notification' in window);
}

// One toggle: enabled -> a click disables, disabled -> a click enables.
// A browser-level block (permission=denied) cannot be lifted from JS, so it
// is explained once. Unsubscribing clears both the browser subscription and
// the server-side binding.
async function toggleNotifications() {
  if (!pushSupported()) {
    toast('Push notifications are not supported in this browser.\nOn iPhone, first add the app to your Home Screen via Safari → Share.');
    return;
  }
  let subscribed = false;
  try {
    const reg = await navigator.serviceWorker.getRegistration();
    if (reg) subscribed = !!(await reg.pushManager.getSubscription());
  } catch {}
  if (Notification.permission === 'granted' && subscribed) {
    // The intent is recorded here rather than in disableNotifications,
    // which logout also calls: signing out must not mean opting out of
    // notifications forever.
    setPushOptOut(true);
    await disableNotifications(false);
  } else {
    setPushOptOut(false);
    await enableNotifications();
  }
}

// silent=true is for logout: best effort, no error messages.
async function disableNotifications(silent) {
  // The token is captured before the first await: during logout it is
  // cleared before the fetch goes out.
  const authToken = (typeof token !== 'undefined' && token) ? token : (localStorage.getItem('hc_token') || '');
  try {
    const reg = await navigator.serviceWorker.getRegistration();
    const sub = reg ? await reg.pushManager.getSubscription() : null;
    if (sub) {
      // Server first (it needs a live subscription endpoint), browser second.
      try {
        await fetch('/api/push/subscribe', {
          method: 'DELETE',
          headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + authToken },
          body: JSON.stringify({ endpoint: sub.endpoint }),
        });
      } catch {}
      await sub.unsubscribe();
    }
    _updateNotifyButton();
  } catch (e) {
    if (!silent) console.error('[push] disable failed', e);
    _updateNotifyButton();
  }
}

async function enableNotifications() {
  if (!pushSupported()) {
    toast('Push notifications are not supported in this browser.\nOn iPhone, first add the app to your Home Screen via Safari → Share.');
    return;
  }
  try {
    const permission = await Notification.requestPermission();
    if (permission !== 'granted') {
      if (permission === 'denied') {
        toast('Notifications are blocked by the browser for this site. Allow them in site settings (lock icon in the address bar) and try again.');
      }
      _updateNotifyButton();
      return;
    }

    const reg = await navigator.serviceWorker.ready;

    // Reuse an existing subscription or create a new one.
    let sub = await reg.pushManager.getSubscription();
    if (!sub) {
      const vapidPublicKey = await fetch('/api/push/vapidPublicKey')
        .then((r) => r.json())
        .then((d) => d.publicKey);
      sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: _urlBase64ToUint8Array(vapidPublicKey),
      });
    }

    // The backend binds the subscription to the user from the JWT, never from
    // the body — so we must send the auth token. (username is sent only as a
    // harmless hint; the server ignores it.)
    const payload = sub.toJSON();
    payload.username = (typeof myUsername !== 'undefined' && myUsername) ? myUsername : null;

    const authToken = (typeof token !== 'undefined' && token) ? token : (localStorage.getItem('hc_token') || '');
    const resp = await fetch('/api/push/subscribe', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Authorization': 'Bearer ' + authToken,
      },
      body: JSON.stringify(payload),
    });
    if (!resp.ok) throw new Error('subscribe HTTP ' + resp.status);

    console.log('[push] subscribed OK');
    _updateNotifyButton();
  } catch (e) {
    console.error('[push] enable failed', e);
  }
}

// Is the server-side push backend actually available? Returns true only if
// /api/push/vapidPublicKey answers with real JSON containing a publicKey. On a
// server without the push endpoints that path falls through to index.html
// (HTML), so this stays false and the button hides — no broken button in prod.
async function _pushBackendAvailable() {
  try {
    const r = await fetch('/api/push/vapidPublicKey', { cache: 'no-store' });
    if (!r.ok) return false;
    const d = JSON.parse(await r.text()); // throws on HTML
    return !!(d && d.publicKey);
  } catch { return false; }
}

// Reflect current permission/subscription state on the sidebar button.
async function _updateNotifyButton() {
  const btn = document.getElementById('notify-btn');
  if (!btn) return;
  if (!pushSupported()) { btn.style.display = 'none'; return; }
  // Only surface the button where the push backend exists (local dev now, and
  // the production server once its /api/push endpoints are deployed).
  if (!(await _pushBackendAvailable())) { btn.style.display = 'none'; return; }
  btn.style.display = '';
  let subscribed = false;
  try {
    const reg = await navigator.serviceWorker.getRegistration();
    if (reg) subscribed = !!(await reg.pushManager.getSubscription());
  } catch {}
  const granted = Notification.permission === 'granted';
  const on = granted && subscribed;
  btn.classList.toggle('notify-on', on);
  const label = btn.querySelector('.notify-label');
  if (label) label.textContent = on ? 'Notifications: on' : 'Notifications: off';
  btn.title = on ? 'Click to turn off push notifications' : 'Click to turn on push notifications';
}

window.addEventListener('load', () => { setTimeout(_updateNotifyButton, 800); setTimeout(refreshNotifyState, 900); });

// ensurePushSubscribed quietly subscribes when permission is already granted
// and the backend is reachable. The Notifications button is therefore
// optional: permission is asked for once at sign-in, and the actual
// subscription to background push happens automatically. The menu toggle
// remains as the way to turn it off.
async function ensurePushSubscribed() {
  if (!pushSupported() || Notification.permission !== 'granted') return;
  // The user switched notifications off — do not turn them back on.
  if (pushOptedOut()) { refreshNotifyState(); return; }
  try {
    if (!(await _pushBackendAvailable())) return;
    const reg = await navigator.serviceWorker.getRegistration();
    if (reg && (await reg.pushManager.getSubscription())) { refreshNotifyState(); return; }
  } catch { return; }
  await enableNotifications(); // permission granted -> subscribes without a prompt
  refreshNotifyState();
}

// refreshNotifyState reflects on/off in the "Notifications" menu item.
async function refreshNotifyState() {
  const item = document.getElementById('settings-notify');
  if (!item) return;
  let subscribed = false;
  try {
    const reg = await navigator.serviceWorker.getRegistration();
    if (reg) subscribed = !!(await reg.pushManager.getSubscription());
  } catch {}
  const on = pushSupported() && Notification.permission === 'granted' && subscribed;
  const lbl = item.querySelector('.dd-label');
  if (lbl) lbl.textContent = on ? 'Notifications: on' : 'Notifications: off';
  item.classList.toggle('on', on);
}
