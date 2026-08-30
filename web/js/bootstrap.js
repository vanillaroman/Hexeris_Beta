// Hexeris — page start-up scripts.
//
// These used to live as inline <script> in index.html. They were moved into a
// file so that Content-Security-Policy can do without script-src 'unsafe-inline':
// with it, any injected <script> would execute as native code.
// The code itself did not change — it only moved; the load order is the same
// (after the other modules, before main.js where that mattered).

// Defaults until /api/config answers (it overwrites them below).
window.APP_NAME = "Hexeris";

// ── block 1 from index.html ──
// ── Viewport height ─────────────────────────────────────────────────────
  // Height is handled purely in CSS via `100dvh` (see #chat-screen). The
  // dynamic viewport unit already excludes mobile browser toolbars, and the
  // on-screen keyboard is handled by `interactive-widget=resizes-content` in
  // the viewport meta (Chrome/Android/Firefox shrink the layout viewport, so
  // dvh + the flex layout keep the composer above the keyboard on their own).
  // iOS Safari — which ignores interactive-widget — is covered by the
  // keyboard-aware scroll in gestures.js, which keeps the latest message in
  // view. No JS-driven pixel height: reacting to every visualViewport scroll
  // made the fixed container thrash while the address bar showed/hid.

  // Register the service worker after load so it can never delay or block the
  // app boot. Fully guarded: if SW is unsupported (older browsers, some WebViews)
  // this silently no-ops and the app runs exactly as before.
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(function () { /* non-fatal */ });
    });
  }

// ── block 2 from index.html ──
// Client configuration from the server (/api/config + /auth/oidc/status).
//
// Applying it moved to js/appconfig.js and became an idempotent function:
// it used to be a piece of the fetch handler right here, applied once in the
// lifetime of the page and only "in one direction" (show). Because of that,
// enabling or disabling any capability on the server only reached the person
// after F5 — see the header of appconfig.js.
//
// configReady — a promise that the public config has been applied to the
// markup. The sign-in screen waits for it, otherwise the form renders BEFORE
// the response and the Sign in / Sign up tabs and the Google block are inserted
// afterwards — and the card visibly jumps. Waiting forever is not an option
// either: if the server does not answer, signing in with a password must still
// be possible.
window.configReady = new Promise(function (resolve) {
  window._configDone = resolve;
  setTimeout(resolve, 1200); // ceiling on the wait
});

refreshAppConfig(true).finally(function () {
  if (window._configDone) window._configDone();
});

// ── block 3 from index.html ──
// ── Composer "+" attach menu ──────────────────────────────────────────────
(function () {
  // photo/video/audio narrow the picker for convenience; document/file — any type.
  const ACCEPT = {
    photo: 'image/*',
    video: 'video/*',
    audio: 'audio/*'
  };

  window.toggleAttachMenu = function (e) {
    if (e) e.stopPropagation();
    const menu = document.getElementById('attach-menu');
    const btn  = document.getElementById('attach-btn');
    const open = menu.classList.toggle('open');
    btn.classList.toggle('open', open);
  };
  window.closeAttachMenu = function () {
    document.getElementById('attach-menu')?.classList.remove('open');
    document.getElementById('attach-btn')?.classList.remove('open');
  };
  window.pickAttach = function (kind) {
    const input = document.getElementById('file-input');
    if (ACCEPT[kind]) input.setAttribute('accept', ACCEPT[kind]);
    else input.removeAttribute('accept');           // document/file → any type
    closeAttachMenu();
    input.click();
    setTimeout(() => input.removeAttribute('accept'), 500); // back to "any file"
  };
  document.addEventListener('click', (e) => {
    if (!e.target.closest('.attach-wrap')) closeAttachMenu();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      closeAttachMenu();
      // Close any open transient UI: context menu, emoji picker, group panels.
      document.getElementById('ctx-menu')?.classList.remove('visible');
      document.getElementById('emoji-picker')?.classList.remove('visible');
      if (typeof cancelReply === 'function') cancelReply();
      if (typeof cancelEdit === 'function' && editingMsg) cancelEdit();
      // Modals (profile, group, network test): Escape used not to close them —
      // only a click on the ✕ or the overlay did.
      if (typeof closeChatSearch === 'function') closeChatSearch();
      if (typeof closeMyProfile === 'function') closeMyProfile();
      if (typeof closeGroupModal === 'function') closeGroupModal();
      if (typeof closeGroupPanel === 'function') closeGroupPanel();
      if (typeof closeNetworkTest === 'function') closeNetworkTest();
      // The list was not closed completely. #ctx-menu is dismissed above — that
      // is the MESSAGE menu — while the chat-list menu (#chat-ctx-menu, on
      // right-click and long press) kept hanging around and survived the next
      // window opening: both the menu and the card on top of it ended up on
      // screen. Another person's profile, forwarding and 2FA did not close at
      // all, so Escape worked for some windows and there was no way to guess
      // which.
      if (typeof closeChatMenu === 'function') closeChatMenu();
      if (typeof closePeerProfile === 'function') closePeerProfile();
      if (typeof closeForward === 'function') closeForward();
      if (typeof dismiss2FAOnEscape === 'function') dismiss2FAOnEscape();
    }
    // Cmd/Ctrl+K → focus search
    if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
      const s = document.getElementById('search-input');
      if (s) { e.preventDefault(); s.focus(); s.select(); }
    }
    // ArrowUp in an empty composer → edit your last message (Slack/iMessage-style).
    if (e.key === 'ArrowUp') {
      const ta = document.getElementById('msg-textarea');
      if (ta && document.activeElement === ta && ta.value === '' && !editingMsg
          && typeof activePeer !== 'undefined' && activePeer
          && typeof editLastOwnMessage === 'function') {
        if (editLastOwnMessage()) e.preventDefault();
      }
    }
  });

  // Platform-correct shortcut hint (⌘K on mac, Ctrl K elsewhere).
  const kbd = document.getElementById('search-kbd');
  if (kbd && !/Mac|iPhone|iPad/.test(navigator.platform)) kbd.textContent = 'Ctrl K';

  // Composer drag&drop affordance (actual drop handled by the chat-area overlay).
  const area = document.querySelector('.input-area');
  if (area) {
    ['dragenter', 'dragover'].forEach(ev => area.addEventListener(ev, e => { e.preventDefault(); area.classList.add('dragover'); }));
    ['dragleave', 'drop'].forEach(ev => area.addEventListener(ev, e => {
      if (ev === 'drop') e.preventDefault();
      if (ev === 'dragleave' && area.contains(e.relatedTarget)) return;
      area.classList.remove('dragover');
    }));
    area.addEventListener('drop', e => { [...(e.dataTransfer?.files || [])].forEach(f => window.enqueueFile && enqueueFile(f)); });
  }
})();


// ── Alternative sign-in methods ────────────────────────────────────────────

// The "or" divider is now owned by applyAppConfig (js/appconfig.js): it shows
// the divider when at least one alternative sign-in method is enabled and HIDES
// it when none is left. The previous revealAltAuth could only show, so a
// disabled method left the divider hanging on its own.

// whenGoogleButtonReady waits for an <iframe> to appear inside the GSI
// container.
//
// The Google library gives no "button rendered" event, so we watch for the node
// insertion. The observer is removed as soon as it fires, and a safety timer
// guarantees the block shows up even if GSI never arrives at all: staying
// invisible forever is worse than showing up empty.
//
// A second and a half, not "a bit more just in case": in an air-gapped network
// accounts.google.com is unreachable and the button will NEVER arrive, and all
// that time an empty strip would hang on the form. The container holds its
// height and dark background, so the wait looks like a placeholder for a
// button rather than a failure.
function whenGoogleButtonReady(cb) {
  var host = document.querySelector('#google-auth-block .g_id_signin');
  if (!host) { cb(); return; }
  if (host.querySelector('iframe')) { cb(); return; }

  var done = false;
  var finish = function () {
    if (done) return;
    done = true;
    try { obs.disconnect(); } catch (e) {}
    clearTimeout(timer);
    cb();
  };
  var obs = new MutationObserver(function () {
    if (host.querySelector('iframe')) finish();
  });
  obs.observe(host, { childList: true, subtree: true });
  var timer = setTimeout(finish, 1500);
}
