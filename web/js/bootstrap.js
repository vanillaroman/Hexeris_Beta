// Hexeris — page startup scripts.
//
// These used to be inline <script> blocks in index.html. They live in a file
// so the Content-Security-Policy can do without script-src 'unsafe-inline',
// which would let any injected <script> execute as first-party code.

// Defaults until /api/config answers and overwrites them below.
window.APP_NAME = "Hexeris";

// ── Block 1 ──
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

// ── Block 2 ──
// Public client configuration from the server (/api/config): the app name and
// the call and Google feature flags.
//
// configReady is the promise that the public config has been applied to the
// markup. The sign-in screen waits for it, or the form renders before the
// answer and the Sign in / Sign up tabs and the Google block are inserted
// afterwards, visibly jolting the card. Waiting forever is not an option
// either: if /api/config never answers, signing in with a password must
// still work.
window.configReady = new Promise(function (resolve) {
  window._configDone = resolve;
  setTimeout(resolve, 1200); // upper bound on the wait
});

  fetch('/api/config').then(function (r) { return r.json(); }).then(function (c) {
    if (c && c.appName) {
      window.APP_NAME = c.appName;
      try { document.title = c.appName; } catch (e) {}
      document.querySelectorAll('.js-app-name').forEach(function (el) { el.textContent = c.appName; });
      var an = document.querySelector('meta[name="application-name"]'); if (an) an.setAttribute('content', c.appName);
    }
    // With public registration disabled, hide the "Sign up" tab and stay on
    // sign-in. The tab row is hidden by default in CSS and shown only when
    // registration is enabled, so the form never displays a lone tab or
    // flickers during load.
    if (c && c.registrationEnabled === true) {
      var tabs = document.querySelector('.auth-tabs'); if (tabs) tabs.style.display = 'flex';
    } else if (typeof switchTab === 'function') {
      switchTab('login');
    }
    if (c && c.callsEnabled) { var cb = document.getElementById('call-buttons'); if (cb) cb.style.display = 'contents'; }
    if (c && c.googleClientId) {
      var go = document.getElementById('g_id_onload'); if (go) go.setAttribute('data-client_id', c.googleClientId);
      var gb = document.getElementById('google-auth-block'); if (gb) gb.style.display = '';
      var sc = document.createElement('script'); sc.src = 'https://accounts.google.com/gsi/client'; sc.async = true; sc.defer = true; document.head.appendChild(sc);
    }
    if (window._configDone) window._configDone();
  }).catch(function () {
    /* non-fatal: without the config we keep the defaults */
    if (window._configDone) window._configDone();
  });

// ── Block 3 ──
// ── Composer "+" attach menu ──────────────────────────────────────────────
(function () {
  // photo/video/audio narrow the picker for convenience; document/file take any type.
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
      // Modals (profile, group, network test): Escape did not close them,
      // only a click on the cross or the overlay did.
      if (typeof closeChatSearch === 'function') closeChatSearch();
      if (typeof closeMyProfile === 'function') closeMyProfile();
      if (typeof closeGroupModal === 'function') closeGroupModal();
      if (typeof closeGroupPanel === 'function') closeGroupPanel();
      if (typeof closeNetworkTest === 'function') closeNetworkTest();
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
