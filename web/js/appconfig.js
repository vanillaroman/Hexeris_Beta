// Client configuration that stays alive rather than being read once.
//
// ═══ WHAT WAS WRONG ═══════════════════════════════════════════════════════
//
// The whole runtime config (application name, calls, Google, public
// registration, SSO) was fetched ONCE on page load and applied imperatively,
// straight into the markup, mixed in with the start-up code. Three troubles
// followed, and all three looked the same to a person — "you need to reload":
//
//   1. Nobody ever re-read the config. Neither a socket reconnect nor coming
//      back to the tab did. The only way to learn that an administrator had
//      enabled calls was F5.
//
//   2. Applying was ONE-WAY: `if (enabled) show()` without `else hide()`. That
//      is, even a repeat call would not have turned anything off — a disabled
//      capability stayed on screen until a reload.
//
//   3. There was nowhere to re-read from either: the applying code was not a
//      function but a piece of a fetch handler inside bootstrap.js.
//
// ═══ WHAT IT IS NOW ═══════════════════════════════════════════════════════
//
// applyAppConfig is an ordinary idempotent function that works in both
// directions. refreshAppConfig re-reads the state and calls it. It is called
// wherever the state COULD have changed:
//
//   • at page start-up;
//   • on every socket (re)connection — a server restart with a new flag shows up
//     exactly like that, and it is the most common case;
//   • when the tab is returned to — no more than once every 10 seconds.
//
// The cost is two responses of a few hundred bytes with no-cache. If nothing
// changed the DOM is not touched at all: a signature of the previous state is
// compared, otherwise every reconnect would repaint the buttons before your eyes.

'use strict';

// The last applied state — so the markup is not touched for nothing.
let _cfgSignature = null;
// The moment of the last re-read: returning to the tab can be frequent
// (Alt-Tab), and there is no reason to turn it into a stream of requests.
let _cfgLastFetch = 0;
const CFG_MIN_INTERVAL_MS = 10000;
// The Google script is loaded exactly once per page lifetime: inserting the
// <script> again would create a second copy of the library.
let _gsiInjected = false;

window.APP_CONFIG = {};

// ── Applying ──────────────────────────────────────────────────────────────
//
// applyAppConfig brings the markup in line with the state.
//
// Every branch must have an else: what is enabled we enable, what is disabled we
// DISABLE. That is exactly what was missing — a capability disabled on the
// server stayed on screen, and pressing it led to a refusal.
function applyAppConfig(c, sso) {
  c = c || {};
  sso = sso || {};
  window.APP_CONFIG = { ...c, ssoEnabled: !!sso.enabled };

  // Application name.
  if (c.appName) {
    window.APP_NAME = c.appName;
    try { document.title = c.appName; } catch (e) {}
    document.querySelectorAll('.js-app-name').forEach(function (el) { el.textContent = c.appName; });
    const an = document.querySelector('meta[name="application-name"]');
    if (an) an.setAttribute('content', c.appName);
  }

  // Public registration: the Sign in / Sign up tab row.
  const tabs = document.querySelector('.auth-tabs');
  if (tabs) {
    if (c.registrationEnabled === true) {
      tabs.style.display = 'flex';
    } else {
      tabs.style.display = 'none';
      // Registration was turned off while the person stood on the "Sign up"
      // tab — leaving them there means leaving them on a form the server rejects.
      if (typeof authMode !== 'undefined' && authMode === 'register' &&
          typeof switchTab === 'function') {
        switchTab('login');
      }
    }
  }

  // Calls.
  const cb = document.getElementById('call-buttons');
  if (cb) cb.style.display = c.callsEnabled ? 'contents' : 'none';

  // Sign in with Google.
  const gb = document.getElementById('google-auth-block');
  if (c.googleClientId) {
    const go = document.getElementById('g_id_onload');
    if (go) go.setAttribute('data-client_id', c.googleClientId);
    if (gb) gb.style.display = '';
    if (!_gsiInjected) {
      _gsiInjected = true;
      const sc = document.createElement('script');
      sc.src = 'https://accounts.google.com/gsi/client';
      sc.async = true;
      sc.defer = true;
      document.head.appendChild(sc);
      // The block is revealed only once GSI has actually rendered the button.
      // Without this the form holds an empty space for a second and then the
      // button appears with a jerk — the very "flicker" of the sign-in screen.
      if (typeof whenGoogleButtonReady === 'function') {
        whenGoogleButtonReady(function () { if (gb) gb.classList.add('ready'); });
      } else if (gb) {
        gb.classList.add('ready');
      }
    }
  } else if (gb) {
    gb.style.display = 'none';
  }

  // Sign-in through a corporate provider.
  const ssoBlock = document.getElementById('sso-block');
  const ssoBtn = document.getElementById('sso-btn');
  if (ssoBlock) {
    if (sso.enabled) {
      if (ssoBtn && sso.label) ssoBtn.textContent = sso.label;
      ssoBlock.style.display = '';
    } else {
      ssoBlock.style.display = 'none';
    }
  }

  // The "or" divider — one for all the alternative methods. Shown only when at
  // least one exists: otherwise it would hang under the password field on its
  // own.
  const sep = document.getElementById('alt-auth-sep');
  if (sep) sep.style.display = (c.googleClientId || sso.enabled) ? 'flex' : 'none';
}

// ── Re-reading ────────────────────────────────────────────────────────────
//
// refreshAppConfig fetches the state and applies it if it changed.
//
// force=true skips the rate limit: page start-up and a socket connection are
// rare and unambiguously significant events.
// Returns true if the markup actually changed.
async function refreshAppConfig(force) {
  const now = Date.now();
  if (!force && now - _cfgLastFetch < CFG_MIN_INTERVAL_MS) return false;
  _cfgLastFetch = now;

  // Both requests in parallel: they are independent, and sequential round-trips
  // at page start are visible to the eye.
  const [cfg, sso] = await Promise.all([
    fetch('/api/config').then(r => r.ok ? r.json() : null).catch(() => null),
    fetch('/auth/oidc/status').then(r => r.ok ? r.json() : null).catch(() => null),
  ]);
  // Neither answered — the network is down. We keep what is already applied:
  // dimming buttons because of a network failure is worse than showing them for
  // an extra minute.
  if (cfg === null && sso === null) return false;

  const sig = JSON.stringify([cfg, sso]);
  if (sig === _cfgSignature) return false;
  const first = _cfgSignature === null;
  _cfgSignature = sig;
  applyAppConfig(cfg, sso);
  if (!first) console.info('[hexeris] configuration updated without a page reload');
  return true;
}

// Returning to the tab is a cheap and natural reason to check: while the tab
// sat in the background an administrator could have switched something.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') refreshAppConfig(false);
});
