// Hexeris — employee profiles: name, position, avatar and presence.
//
// online/offline comes separately from onlineStatuses (a live WS connection);
// presence is the status the user sets themselves (available/busy/away).

const profiles = {};
let myProfile = { display_name: '', position: '', avatar_url: '', email: '', phone: '', presence: 'available' };

const PRESENCE_META = {
  available: { label: 'online', color: 'var(--online)' },
  busy:      { label: 'busy',   color: '#d9534f' },
  away:      { label: 'away',   color: '#e0a63c' },
};

function storeProfile(p) {
  if (!p || !p.username) return;
  profiles[p.username] = {
    display_name: p.display_name || '',
    position: p.position || '',
    avatar_url: p.avatar_url || '',
    presence: p.presence || 'available',
  };
  if (typeof p.online === 'boolean') onlineStatuses[p.username] = p.online;
}

// Load one's own profile and every peer's in a single request.
async function loadProfiles() {
  const auth = { headers: { 'Authorization': 'Bearer ' + token } };
  try {
    const me = await fetch(`${location.protocol}//${SERVER}/api/profile`, auth).then(r => r.ok ? r.json() : null);
    if (me) { myProfile = me; storeProfile(me); }
  } catch {}
  try {
    const list = await fetch(`${location.protocol}//${SERVER}/api/profiles`, auth).then(r => r.ok ? r.json() : []);
    for (const p of (list || [])) storeProfile(p);
  } catch {}
  renderMe();
  if (typeof renderContacts === 'function') renderContacts();
  if (activePeer && typeof showChatHeader === 'function') showChatHeader(activePeer);
}

// Fetch one peer's profile (when a new chat is opened).
async function fetchPeerProfile(peer) {
  // A deleted account is not queried: the answer is known in advance — 404.
  if (typeof peerIsGone === 'function' && peerIsGone(peer)) return;
  try {
    const p = await fetch(`${location.protocol}//${SERVER}/api/profile?user=${encodeURIComponent(peer)}`,
      { headers: { 'Authorization': 'Bearer ' + token } }).then(r => {
        if (r.status === 404 && typeof markPeerGone === 'function') markPeerGone(peer);
        return r.ok ? r.json() : null;
      });
    if (p) {
      storeProfile(p);
      if (typeof renderContacts === 'function') renderContacts();
      if (activePeer === peer && typeof showChatHeader === 'function') showChatHeader(peer);
    }
  } catch {}
}

// ── Render helpers used by ui.js ─────────────────────────────────────────────

// avatarImg returns the avatar <img> when one is loaded, otherwise an empty
// string, in which case the caller renders the letter circle.
function avatarImg(peer) {
  const p = profiles[peer];
  const url = p && p.avatar_url;
  // draggable="false" and an empty alt: the browser must neither drag the
  // avatar away with the mouse nor show a caption. The long press on a phone
  // that opens the system "save image" menu is closed off in CSS —
  // -webkit-touch-callout: none on .av-img.
  // Through mediaSrc for the same reason as pictures in a conversation: some
  // clients have stale 404s stuck in their cache (see MEDIA_CACHE_BUST in
  // helpers.js), and avatars suffered in exactly the same way.
  return url ? `<img class="av-img" src="${escHtml(mediaSrc(url))}" alt="" draggable="false">` : '';
}

// presenceDotClass combines online state with the chosen presence.
function presenceDotClass(peer) {
  if (!onlineStatuses[peer]) return 'contact-online-dot';
  const pr = (profiles[peer] && profiles[peer].presence) || 'available';
  return 'contact-online-dot online pr-' + pr;
}

// presenceLabel returns the text and colour for the chat header.
function presenceLabel(peer) {
  if (!onlineStatuses[peer]) return { text: 'offline', color: 'var(--muted)' };
  const pr = (profiles[peer] && profiles[peer].presence) || 'available';
  const m = PRESENCE_META[pr] || PRESENCE_META.available;
  return { text: m.label, color: m.color };
}

// ── Own card in the sidebar ──────────────────────────────────────────────────
function renderMe() {
  const label = document.getElementById('me-label');
  if (label) label.textContent = myProfile.display_name || myUsername || 'you';
  const pos = document.getElementById('me-position');
  if (pos) pos.textContent = myProfile.position || '';
  const av = document.getElementById('me-avatar');
  if (av) {
    av.className = 'me-avatar ' + (myUsername ? avatarClass(myUsername) : 'av-0') + (myProfile.avatar_url ? ' has-img' : '');
    av.innerHTML = myProfile.avatar_url
      ? `<img class="av-img" src="${escHtml(myProfile.avatar_url)}" alt="">`
      : escHtml((myProfile.display_name || myUsername || '?')[0].toUpperCase());
  }
  const dot = document.getElementById('me-dot');
  if (dot) {
    const m = PRESENCE_META[myProfile.presence] || PRESENCE_META.available;
    dot.style.background = m.color;
  }
}

// ── WS: an incoming profile update from a peer ──────────────────────────────
function handleProfileMsg(msg) {
  // The server broadcasts a profile only for a live account — so the person is
  // there. We clear the "deleted" mark if it lingered from an earlier life of
  // this username.
  if (typeof unmarkPeerGone === 'function') unmarkPeerGone(msg.from);
  storeProfile({
    username: msg.from,
    display_name: msg.display_name, position: msg.position,
    avatar_url: msg.avatar_url, presence: msg.presence,
  });
  if (typeof renderContacts === 'function') renderContacts();
  if (activePeer === msg.from && typeof showChatHeader === 'function') showChatHeader(activePeer);
}

// ── Editing one's own profile ───────────────────────────────────────────────
let _pfPresence = 'available';

function openMyProfile() {
  const ov = document.getElementById('profile-modal-overlay');
  if (!ov) return;
  document.getElementById('pf-name').value = myProfile.display_name || '';
  document.getElementById('pf-position').value = myProfile.position || '';
  document.getElementById('pf-email').value = myProfile.email || '';
  document.getElementById('pf-phone').value = myProfile.phone || '';
  _pfPresence = myProfile.presence || 'available';
  renderPfAvatar(myProfile.avatar_url);
  highlightPresence();
  ov.classList.add('open');
}

function closeMyProfile() {
  const ov = document.getElementById('profile-modal-overlay');
  if (ov) ov.classList.remove('open');
}

// ── Viewing a peer's profile (read-only) ──
async function openPeerProfile(peer) {
  if (!peer || peer.startsWith('g:')) return;   // a group is not an employee card
  const ov = document.getElementById('peer-profile-overlay');
  if (!ov) return;
  fillPeerProfile(peer, profiles[peer] || {});   // instantly from cache (name/position)
  // The card opens from the cache FIRST and refreshes afterwards. Waiting for
  // the request would make a click do nothing visible until the network came
  // back with an answer.
  ov.classList.add('open');
  try {                                          // then refresh: email/phone only via GET
    const r = await fetch(`${location.protocol}//${SERVER}/api/profile?user=${encodeURIComponent(peer)}`,
      { headers: { 'Authorization': 'Bearer ' + token } });
    // The card is opened by hand and rarely — so the request always goes out,
    // even when the account is marked deleted. In return its response refreshes
    // our memory: the username could have been registered again, and one
    // explicit click clears the mark ahead of time.
    if (r.status === 404 && typeof markPeerGone === 'function') markPeerGone(peer);
    if (r.ok) {
      if (typeof unmarkPeerGone === 'function') unmarkPeerGone(peer);
      fillPeerProfile(peer, await r.json());
    }
  } catch {}
}

function fillPeerProfile(peer, p) {
  const nm = p.display_name || peer;
  document.getElementById('ppf-name').textContent = nm;
  document.getElementById('ppf-position').textContent = p.position || '';
  const av = document.getElementById('ppf-avatar');
  const img = (typeof avatarImg === 'function') ? avatarImg(peer) : '';
  av.className = 'pf-avatar ' + (typeof avatarClass === 'function' ? avatarClass(peer) : 'av-0') + (img ? ' has-img' : '');
  av.innerHTML = img || escHtml((nm || '?')[0].toUpperCase());
  // The avatar can be examined — but only a real one: there is nothing to open
  // on a letter circle, and a click on it must not pretend otherwise.
  const avURL = (p && p.avatar_url) || '';
  if (avURL) {
    av.dataset.act = 'openMedia';
    av.dataset.a1 = avURL;
    av.dataset.a2 = nm || peer;
    av.setAttribute('aria-label', 'View photo');
    av.style.cursor = 'zoom-in';
  } else {
    delete av.dataset.act; delete av.dataset.a1; delete av.dataset.a2;
    av.removeAttribute('aria-label');
    av.style.cursor = '';
  }
  _pfRow('ppf-email-row', 'ppf-email', p.email, 'mailto:');
  _pfRow('ppf-phone-row', 'ppf-phone', p.phone, 'tel:');
}

function _pfRow(rowId, valId, val, scheme) {
  const row = document.getElementById(rowId), el = document.getElementById(valId);
  if (val) { el.textContent = val; el.href = scheme + val; row.style.display = ''; }
  else { row.style.display = 'none'; }
}

function closePeerProfile() {
  const ov = document.getElementById('peer-profile-overlay');
  if (ov) ov.classList.remove('open');
}

function renderPfAvatar(url) {
  const el = document.getElementById('pf-avatar');
  if (!el) return;
  el.className = 'pf-avatar ' + (myUsername ? avatarClass(myUsername) : 'av-0') + (url ? ' has-img' : '');
  el.innerHTML = url
    ? `<img class="av-img" src="${escHtml(url)}" alt="">`
    : escHtml(((document.getElementById('pf-name').value || myUsername || '?')[0] || '?').toUpperCase());
  el.dataset.url = url || '';
}

function pickPresence(pr) {
  _pfPresence = pr;
  highlightPresence();
}

function highlightPresence() {
  document.querySelectorAll('.pf-pr').forEach(b => {
    b.classList.toggle('active', b.dataset.pr === _pfPresence);
  });
}

async function uploadAvatar(input) {
  const file = input.files && input.files[0];
  input.value = '';
  if (!file) return;
  if (!/^image\//.test(file.type)) { toast('Avatar must be an image'); return; }
  const fd = new FormData();
  fd.append('file', file);
  try {
    const res = await fetch(`${location.protocol}//${SERVER}/upload`, {
      method: 'POST',
      headers: { 'Authorization': 'Bearer ' + token },
      body: fd,
    });
    if (!res.ok) throw new Error(await res.text());
    const data = await res.json();
    renderPfAvatar(data.url); // stores the link in data-url, applied on Save
  } catch (e) {
    toast('Upload failed: ' + (e.message || e));
  }
}

async function saveMyProfile() {
  const btn = document.getElementById('pf-save');
  if (btn) { btn.disabled = true; btn.textContent = 'Saving…'; }
  const body = {
    display_name: document.getElementById('pf-name').value.trim(),
    position: document.getElementById('pf-position').value.trim(),
    email: document.getElementById('pf-email').value.trim(),
    phone: document.getElementById('pf-phone').value.trim(),
    avatar_url: document.getElementById('pf-avatar').dataset.url || '',
  };
  try {
    const auth = { 'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json' };
    const r = await fetch(`${location.protocol}//${SERVER}/api/profile`, {
      method: 'POST', headers: auth, body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(await r.text());
    const p = await r.json();
    // Presence is sent separately, if the user changed it.
    if (_pfPresence && _pfPresence !== (myProfile.presence || 'available')) {
      await fetch(`${location.protocol}//${SERVER}/api/presence`, {
        method: 'POST', headers: auth, body: JSON.stringify({ presence: _pfPresence }),
      });
      p.presence = _pfPresence;
    }
    myProfile = p;
    storeProfile(p);
    renderMe();
    if (typeof renderContacts === 'function') renderContacts();
    if (activePeer && typeof showChatHeader === 'function') showChatHeader(activePeer);
    closeMyProfile();
    toast('Profile saved', 'success');
  } catch (e) {
    toast('Save failed: ' + (e.message || e));
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Save'; }
  }
}
