// Hexeris — small UI utilities: escaping, avatars, typing.

// ── Utils ─────────────────────────────────────────────────
let _typingThrottle = 0;
function sendTyping() {
  if (!activePeer || !ws || ws.readyState !== 1) return;
  const now = Date.now();
  if (now - _typingThrottle < 2000) return; // at most once every two seconds
  _typingThrottle = now;
  ws.send(JSON.stringify({ type: 'typing', from: myUsername, to: activePeer }));
}

function autoResize(el) {
  el.style.height = 'auto';
  el.style.height = Math.min(el.scrollHeight, 120) + 'px';
}

function avatarClass(name) {
  if (name.startsWith('g:')) return 'av-g';
  const i = name.charCodeAt(0) % 5;
  return 'av-' + i;
}

function displayName(peer) {
  if (peer.startsWith('g:')) return groupDisplayName(peer);
  const p = (typeof profiles !== 'undefined') && profiles[peer];
  return (p && p.display_name) ? p.display_name : peer;
}

function escHtml(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

// ── Toast notifications (replace blocking alert()) ────────────────────────────
// Non-blocking, auto-dismissing. type: 'error' (default) | 'success' | 'info'.
function toast(message, type) {
  let host = document.getElementById('toast-host');
  if (!host) {
    host = document.createElement('div');
    host.id = 'toast-host';
    document.body.appendChild(host);
  }
  const el = document.createElement('div');
  el.className = 'toast toast-' + (type || 'error');
  el.textContent = message;
  host.appendChild(el);
  // force reflow so the enter transition runs
  void el.offsetWidth;
  el.classList.add('in');
  setTimeout(() => {
    el.classList.remove('in');
    setTimeout(() => el.remove(), 220);
  }, 3600);
}

// ── Hexeris-styled confirm / prompt (replace native confirm()/prompt()) ───────
function _hexModal(o) {
  return new Promise((resolve) => {
    const ov = document.getElementById('hex-modal-overlay');
    if (!ov) { resolve(o.input ? null : window.confirm(o.message)); return; }
    document.getElementById('hex-modal-title').textContent = o.title || '';
    document.getElementById('hex-modal-msg').textContent = o.message || '';
    const inp = document.getElementById('hex-modal-input');
    inp.style.display = o.input ? 'block' : 'none';
    inp.value = '';
    if (o.input) inp.placeholder = o.placeholder || '';
    const ok = document.getElementById('hex-modal-ok');
    const cancel = document.getElementById('hex-modal-cancel');
    ok.textContent = o.okText || 'OK';
    ok.className = 'hex-btn ' + (o.okClass || 'primary');
    ov.classList.add('open');
    if (o.input) setTimeout(() => inp.focus(), 30);
    const close = (val) => {
      ov.classList.remove('open');
      ok.onclick = cancel.onclick = ov.onclick = inp.onkeydown = null;
      resolve(val);
    };
    ok.onclick = () => close(o.input ? (inp.value.trim() || null) : true);
    cancel.onclick = () => close(o.input ? null : false);
    ov.onclick = (e) => { if (e.target === ov) close(o.input ? null : false); };
    inp.onkeydown = (e) => {
      if (e.key === 'Enter') { e.preventDefault(); ok.onclick(); }
      if (e.key === 'Escape') { e.preventDefault(); cancel.onclick(); }
    };
  });
}
function hexConfirm(message, opts) {
  opts = opts || {};
  return _hexModal({ title: opts.title || 'Confirm', message, okText: opts.okText || 'Confirm', okClass: opts.danger ? 'danger' : 'primary' });
}
function hexPrompt(message, placeholder) {
  return _hexModal({ title: 'Enter', message, input: true, placeholder: placeholder || '', okText: 'OK' });
}

// The real file name lives in the fragment of the message body:
//   /files/<random>.<ext>#<url-encoded name>
// The browser drops the fragment from requests, so server URLs and older
// files without a fragment are unaffected.
function fileUrl(body) {
  const i = (body || '').indexOf('#');
  return i < 0 ? (body || '') : body.slice(0, i);
}
function fileName(body) {
  const i = (body || '').indexOf('#');
  if (i >= 0) {
    try { return decodeURIComponent(body.slice(i + 1)); } catch { return body.slice(i + 1); }
  }
  return decodeURIComponent((body || '').split('/').pop() || 'file'); // older file: the on-disk name
}
