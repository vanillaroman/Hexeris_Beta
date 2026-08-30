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
  // A toast is the only channel through which the app reports a failure
  // ("Message not sent…"). Without announcing it, it stayed purely visual.
  // An error interrupts speech; a confirmation waits for a pause.
  if (typeof announce === 'function') announce(message, (type || 'error') === 'error');
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
// MEDIA_CACHE_BUST — a one-off media cache reset for clients ALREADY running.
//
// For a while, error responses for /files/ went out with the header
// "private, max-age=86400": a browser that once got a 404 remembered it for a
// day and stopped going to the server at all. The server-side cause was fixed,
// but the entries stayed in caches on devices, and from the outside it looks as
// if nothing was fixed.
//
// Raise the number by one so media addresses change and browsers re-request them
// once. Ordinary app updates do not need this — the constant is changed by hand
// and only when that is actually required.
const MEDIA_CACHE_BUST = 1;

// mediaSrc — the address for <img>/<video>: without the file-name fragment and
// with an invalidation tag.
function mediaSrc(body) {
  const u = fileUrl(body);
  if (!u || !MEDIA_CACHE_BUST) return u;
  return u + (u.includes('?') ? '&' : '?') + 'v=' + MEDIA_CACHE_BUST;
}

function fileName(body) {
  const i = (body || '').indexOf('#');
  if (i >= 0) {
    try { return decodeURIComponent(body.slice(i + 1)); } catch { return body.slice(i + 1); }
  }
  return decodeURIComponent((body || '').split('/').pop() || 'file'); // older file: the on-disk name
}

// contactTime — a short stamp for a list row: today the time, yesterday
// "Yesterday", earlier this week the weekday, further back the date.
//
// A contact row used to end with the name and a scrap of preview, leaving empty
// space on the right. The timestamp both fills it and answers the question
// people ask first in a conversation list: when was this.
function contactTime(ts) {
  if (!ts) return '';
  const d = new Date(ts);
  if (isNaN(d)) return '';
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  if (sameDay) {
    return d.getHours().toString().padStart(2, '0') + ':' +
           d.getMinutes().toString().padStart(2, '0');
  }
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (d.toDateString() === y.toDateString()) return 'Yesterday';
  // Within a week the weekday reads faster than a date.
  if (now - d < 7 * 86400000) return d.toLocaleDateString(undefined, { weekday: 'short' });
  return d.toLocaleDateString(undefined, { day: '2-digit', month: '2-digit' });
}

// ── Deleted accounts ──────────────────────────────────────────────────────
//
// A conversation stays in the list even after the other person is deleted: the
// messages have not gone anywhere, and hiding them is not an option. But the
// client kept asking about such a person for presence and profile — two
// requests per contact on every load, and each answered 404. In the console
// that looked like a breakage even though nothing is broken; on the server, like
// extra work growing with the number of conversations.
//
// We remember such names and do not ask again. Memory with an expiry rather than
// forever: a username can be registered again, and an eternal mark would make
// the new person invisible — silently and for a long time.
//
// The rule the calls follow (checked by uitest_gone.js):
//   • automatic requests — presence and profile when a conversation opens —
//     are SKIPPED when the mark is set; that is the whole gain;
//   • requests from an explicit human action — typed a username, forwarded a
//     message, opened a card — always go out, and their answer updates the
//     memory in both directions: that is how the mark is cleared ahead of time
//     if the username came back to life;
//   • a sign of life without a request — an incoming message or a profile
//     broadcast — clears the mark immediately; deleting a user (WS user-deleted)
//     sets it without waiting for the first 404.
//
// The key is shared by all accounts on the device, and that is deliberate: the
// account is deleted for everyone, not only for whoever noticed it first.
const GONE_KEY = 'hc_gone_users';
const GONE_TTL_MS = 12 * 3600 * 1000;

function _goneLoad() {
  try {
    const raw = JSON.parse(localStorage.getItem(GONE_KEY) || '{}');
    const now = Date.now();
    const out = {};
    for (const k in raw) if (now - raw[k] < GONE_TTL_MS) out[k] = raw[k];
    return out;
  } catch (e) { return {}; }
}
let _goneUsers = _goneLoad();

// peerIsGone — whether it is worth asking the server about this peer at all.
// Groups never get here: for /status a group does not exist anyway, and it is
// checked separately by prefix.
function peerIsGone(peer) {
  return !!(peer && _goneUsers[peer]);
}

// markPeerGone — the server answered 404 to a question about a person, or sent
// user-deleted. Groups are not marked: for /status a group "does not exist"
// anyway, and the mark would make its own header incomplete.
function markPeerGone(peer) {
  if (!peer || peer.startsWith('g:')) return;
  _goneUsers[peer] = Date.now();
  try { localStorage.setItem(GONE_KEY, JSON.stringify(_goneUsers)); } catch (e) {}
}

// unmarkPeerGone — they are back (wrote a message, sent a profile, were found
// by an explicit username check). The mark is removed at once without waiting
// for expiry: otherwise a re-registered username stays without a name, avatar
// and presence for up to half a day.
function unmarkPeerGone(peer) {
  if (!peer || !_goneUsers[peer]) return;
  delete _goneUsers[peer];
  try { localStorage.setItem(GONE_KEY, JSON.stringify(_goneUsers)); } catch (e) {}
}
