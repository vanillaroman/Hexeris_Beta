// Hexeris — conversation preferences (mute/archive/clear) and the chat list
// context menu.
//
// The preferences live on the server (see server/chatprefs.go) rather than in
// localStorage next to pinning: mute must suppress a push before it is even
// sent, and a list tidied up on a phone must look the same on a computer.
// The local copy here is only a cache for the first frame: without it the list
// managed to render with all the archived chats on every load and only then
// collapsed.

let chatPrefs = {};   // peer → { muted, archived, cleared_seq }
let showArchived = false;

function _prefsCacheKey() { return 'hc_chatprefs_' + myUsername; }

function _saveChatPrefsCache() {
  try { localStorage.setItem(_prefsCacheKey(), JSON.stringify(chatPrefs)); } catch {}
}

function chatPrefOf(peer) {
  return chatPrefs[peer] || { muted: false, archived: false, cleared_seq: 0, archived_at: 0 };
}
function chatIsMuted(peer)    { return !!chatPrefOf(peer).muted; }
function chatIsArchived(peer) { return !!chatPrefOf(peer).archived; }
// When the conversation was archived (ms). 0 — not archived, or there is no
// stamp (the row was created before archived_at existed).
function chatArchivedAt(peer) { return chatPrefOf(peer).archived_at || 0; }

// loadChatPrefs — the cache first, the server after. The server response is the
// source of truth: it overwrites the cache entirely, otherwise a setting turned
// off from another device would live on here forever.
async function loadChatPrefs() {
  if (!token || !myUsername) return;
  try { chatPrefs = JSON.parse(localStorage.getItem(_prefsCacheKey()) || '{}'); }
  catch { chatPrefs = {}; }
  if (typeof renderContacts === 'function') renderContacts();
  try {
    const r = await fetch(`${location.protocol}//${SERVER}/chats/prefs`, {
      headers: { 'Authorization': `Bearer ${token}` }
    });
    if (!r.ok) return;
    chatPrefs = await r.json() || {};
    _saveChatPrefsCache();
    _applyClearedBoundary();
    if (typeof renderContacts === 'function') renderContacts();
  } catch {}
}

// _applyClearedBoundary — catch up with a deletion made on another device.
//
// After a clear the server stops returning old messages, but on a second device
// they are already in localStorage and its sync cursor is higher — that is, it
// would never learn about the clear on its own. No separate event is needed for
// this: the boundary is visible in the same preferences the device requests on
// every sign-in anyway.
//
// Messages without a seq (still being sent) are left alone: they have no number
// yet, and against the boundary they are formally "older" than everything.
function _applyClearedBoundary() {
  for (const peer of Object.keys(chatPrefs)) {
    const limit = chatPrefs[peer] && chatPrefs[peer].cleared_seq;
    if (!limit) continue;
    const kept = (chats[peer] || []).filter(m => !m.seq || m.seq > limit);
    if (kept.length) {
      chats[peer] = kept;
    } else if (chats[peer]) {
      delete chats[peer];
      if (activePeer === peer && typeof closeActiveChat === 'function') closeActiveChat();
    }
    try {
      const key = `hc_msgs_${myUsername}_${peer}`;
      const stored = JSON.parse(localStorage.getItem(key) || '[]').filter(m => !m.seq || m.seq > limit);
      if (stored.length) localStorage.setItem(key, JSON.stringify(stored));
      else localStorage.removeItem(key);
    } catch {}
  }
}

// setChatPref — change it locally, optimistically, and send it to the server.
// On a refusal we roll back: silently leaving on something the server did not
// accept means showing the user a setting that does not exist.
//
// localOnly — fields the interface needs immediately but which are not sent to
// the server: it computes them itself (archived_at) and returns its own value.
async function setChatPref(peer, patch, localOnly) {
  const before = { ...chatPrefOf(peer) };
  chatPrefs[peer] = { ...before, ...patch, ...(localOnly || {}) };
  _saveChatPrefsCache();
  if (typeof renderContacts === 'function') renderContacts();
  try {
    const r = await fetch(`${location.protocol}//${SERVER}/chats/prefs`, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer, ...patch })
    });
    if (!r.ok) throw new Error(r.status);
    chatPrefs[peer] = await r.json();
  } catch {
    chatPrefs[peer] = before;
    if (typeof toast === 'function') toast('Could not save — check your connection.');
  }
  _saveChatPrefsCache();
  if (typeof renderContacts === 'function') renderContacts();
}

// ── Actions ────────────────────────────────────────────────────────────────

async function toggleChatMute(peer) {
  await setChatPref(peer, { muted: !chatIsMuted(peer) });
  if (typeof toast === 'function') {
    toast(chatIsMuted(peer) ? 'Muted — no notifications from this chat.' : 'Unmuted.', 'success');
  }
}

async function toggleChatArchive(peer) {
  const wasArchived = chatIsArchived(peer);
  // The timestamp is set locally too: while the request is in flight the row
  // must already move to the end of the archive. Without this it slid down for a
  // moment (no stamp — "order unknown") and jumped into place after the response.
  // The server returns its own, and that one becomes final.
  await setChatPref(peer, { archived: !wasArchived },
    { archived_at: wasArchived ? 0 : Date.now() });
  // On going into the archive an open conversation is closed: otherwise it stays
  // on screen while its row is already gone from the list — leaving it unclear
  // what happened.
  if (!wasArchived && activePeer === peer && typeof closeActiveChat === 'function') {
    closeActiveChat();
  }
  if (typeof toast === 'function') {
    toast(wasArchived ? 'Moved out of archive.' : 'Archived.', 'success');
  }
}

// Is the "Archive" section expanded? A function rather than a direct variable:
// ui.js loads earlier than this module, and touching a let binding before its
// initialisation would throw a ReferenceError instead of a quiet "no archive".
function showArchivedList() { return showArchived; }

function toggleArchivedView() {
  showArchived = !showArchived;
  renderContacts();
}

// markChatRead — zeroes the unread counter without opening the conversation.
function markChatRead(peer) {
  if (typeof markPeerRead !== 'function') return;
  markPeerRead(peer);
  // markPeerRead moves the cursor only across loaded messages, and that is
  // enough: unread counts are computed from the very same ones.
  renderContacts();
  if (typeof updatePageTitle === 'function') updatePageTitle();
  if (ws && ws.readyState === 1 && !peer.startsWith('g:')) {
    ws.send(JSON.stringify({ type: 'read', from: myUsername, to: peer }));
  }
}

// deleteChat — "delete for me". The messages are not erased: the server raises
// a personal visibility boundary, the peer keeps the conversation, and it is also
// preserved for an internal investigation. The wording in the confirmation must
// reflect that — otherwise the user believes they wiped the conversation for
// both sides.
async function deleteChat(peer) {
  const name = (typeof displayName === 'function') ? displayName(peer) : peer;
  const ok = (typeof hexConfirm !== 'function') || await hexConfirm(
    'Messages will be removed from your account, on this and your other devices. ' +
    (peer.startsWith('g:') ? 'Other members' : name) + ' will still have their copy.',
    { title: 'Delete chat?', okText: 'Delete', danger: true });
  if (!ok) return;

  // A network failure and a server refusal are different troubles, and the
  // advice for them differs. Both paths used to converge in one catch and the
  // connection was always blamed, even though a perfectly definite answer could
  // have been received.
  let r;
  try {
    r = await fetch(`${location.protocol}//${SERVER}/chats/clear`, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer })
    });
  } catch {
    if (typeof toast === 'function') toast('Could not delete — check your connection.');
    return;
  }
  if (!r.ok) {
    if (typeof toast === 'function') {
      toast(r.status === 401 ? 'Could not delete — please sign in again.'
                             : 'Could not delete — the server refused (' + r.status + ').');
    }
    return;
  }
  try {
    const res = await r.json();
    chatPrefs[peer] = { ...chatPrefOf(peer), cleared_seq: res.cleared_seq || 0 };
    _saveChatPrefsCache();
    // The peer does not exist on the server. We mark them — the client then
    // stops asking for their presence and profile (helpers.js).
    if (res.gone && typeof markPeerGone === 'function') markPeerGone(peer);
  } catch {
    // The body is unreadable but the status is a success: we remove the conversation and leave the boundary alone.
  }

  // Local traces of the conversation are removed only after the server succeeds:
  // otherwise a network error would make the conversation vanish from the screen
  // and come back on the next sync.
  _forgetChatLocally(peer);
  renderContacts();
  if (typeof updatePageTitle === 'function') updatePageTitle();
  if (typeof toast === 'function') toast('Chat deleted.', 'success');
}

// _forgetChatLocally — erase the traces of a conversation on THIS device.
// Split out of deleteChat because the phantom-row cleanup below uses the same
// set: not only the messages have to be forgotten but the cursors too, otherwise
// the sync later decides part of the history is already fetched and leaves a hole.
function _forgetChatLocally(peer) {
  delete chats[peer];
  try { localStorage.removeItem(`hc_msgs_${myUsername}_${peer}`); } catch {}
  if (typeof lastSeqByPeer === 'object' && lastSeqByPeer) {
    delete lastSeqByPeer[peer];
    if (typeof saveLastSeq === 'function') saveLastSeq();
  }
  if (typeof lastReadSeq === 'object' && lastReadSeq) {
    delete lastReadSeq[peer];
    if (typeof saveLastReadSeq === 'function') saveLastReadSeq();
  }
  if (activePeer === peer && typeof closeActiveChat === 'function') closeActiveChat();
}

// ── Cleaning up conversations with deleted accounts ───────────────────────
//
// ═══ WHERE THESE ROWS COME FROM ══════════════════════════════════════════
//
// Deleting an employee wipes their whole conversation and broadcasts
// user-deleted to their peers — but only to those ONLINE at that second.
// Whoever was offline never finds out: on the server there is nothing left
// about that conversation, while the messages sit in the browser cache, and
// loadHistory builds the conversation list from the cache too. The row stays
// forever and leads nowhere.
//
// ═══ WHY THIS IS DONE ON REQUEST RATHER THAN BY ITSELF ═══════════════════
//
// The browser cache is the only remaining copy of that conversation. Wiping it
// silently, on our own guess, means destroying data the person never asked to
// lose. So: we ask, show the list, and wipe only after consent.
//
// The cleanup is local: there is nothing to sync it with — those conversations
// are already gone from the server, and on another device the same cache has to
// be cleared separately.

const CLEANUP_PROBE_PARALLEL = 4;   // simultaneous probes

// _peerGoneOnServer — a probe for one username. Returns 'gone', 'alive' or
// 'unknown'. Not knowing is a full answer: nothing is deleted on it.
async function _peerGoneOnServer(peer) {
  try {
    const r = await fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(peer)}`,
      { headers: { 'Authorization': `Bearer ${token}` } });
    if (r.status === 404) return 'gone';
    if (r.ok) return 'alive';
    return 'unknown';   // a 429 from the limiter, a 5xx, anything else
  } catch {
    return 'unknown';
  }
}

async function cleanupDeletedChats() {
  const peers = Object.keys(chats || {}).filter((p) => !p.startsWith('g:') && p !== myUsername);
  if (!peers.length) {
    if (typeof toast === 'function') toast('No chats to check.');
    return;
  }
  if (typeof toast === 'function') toast('Checking ' + peers.length + ' chats…');

  const gone = [];
  let unknown = 0;
  const queue = peers.slice();
  // In batches rather than all at once: /status is limited to 200 requests per
  // 10 minutes, and a volley of a hundred parallel connections would hit the
  // limiter and leave half the list unchecked.
  await Promise.all(Array.from({ length: Math.min(CLEANUP_PROBE_PARALLEL, queue.length) }, async () => {
    while (queue.length) {
      const peer = queue.shift();
      const verdict = await _peerGoneOnServer(peer);
      if (verdict === 'gone') gone.push(peer);
      else if (verdict === 'unknown') unknown++;
    }
  }));

  if (!gone.length) {
    if (typeof toast === 'function') {
      toast(unknown ? 'No deleted accounts found, but ' + unknown + ' could not be checked.'
                    : 'No deleted accounts in your chat list.');
    }
    return;
  }

  // The question names them all if there are only a few: "delete 7 conversations"
  // without a list is an invitation to agree blindly.
  const shown = gone.slice(0, 10).join(', ') + (gone.length > 10 ? ', …' : '');
  const ok = (typeof hexConfirm !== 'function') || await hexConfirm(
    gone.length + (gone.length === 1 ? ' account no longer exists' : ' accounts no longer exist') +
    ': ' + shown + '.\n\n' +
    'Their messages are already gone from the server — the copy in this browser is the last one. ' +
    'Removing these chats deletes that copy on this device.',
    { title: 'Remove deleted accounts?', okText: 'Remove', danger: true });
  if (!ok) return;

  for (const peer of gone) {
    if (typeof markPeerGone === 'function') markPeerGone(peer);
    _forgetChatLocally(peer);
  }
  renderContacts();
  if (typeof updatePageTitle === 'function') updatePageTitle();
  if (typeof toast === 'function') {
    toast('Removed ' + gone.length + (gone.length === 1 ? ' chat.' : ' chats.') +
          (unknown ? ' ' + unknown + ' could not be checked.' : ''), 'success');
  }
}

// ── Chat list context menu ─────────────────────────────────────────────────
//
// Right-click on a desktop and a long press on a phone. On Android a long press
// produces contextmenu by itself, on iOS it does not, so a separate timer is
// needed; _ctxOpenedAt protects against both paths firing on Android and opening
// the menu twice.

let ctxChatPeer = null;
let _ctxOpenedAt = 0;

function showChatMenu(e, peer) {
  if (e && e.preventDefault) e.preventDefault();
  if (!peer) return;
  ctxChatPeer = peer;
  _ctxOpenedAt = Date.now();

  const menu = document.getElementById('chat-ctx-menu');
  if (!menu) return;

  const pinned = (typeof pinnedSet === 'function') && pinnedSet().has(peer);
  const unread = (typeof countUnread === 'function') ? countUnread(peer) : 0;
  _setMenuItem(menu, 'cctx-pin', pinned ? 'Unpin' : 'Pin to top');
  _setMenuItem(menu, 'cctx-mute', chatIsMuted(peer) ? 'Unmute' : 'Mute');
  _setMenuItem(menu, 'cctx-archive', chatIsArchived(peer) ? 'Move out of archive' : 'Archive');
  const readItem = menu.querySelector('#cctx-read');
  if (readItem) readItem.style.display = unread > 0 ? 'flex' : 'none';

  menu.classList.add('visible');
  // The position is at the cursor/finger, clamped inside the window so the menu
  // does not run off the bottom edge on a phone.
  const pt = (e && e.touches && e.touches[0]) || e || { clientX: 0, clientY: 0 };
  const rect = menu.getBoundingClientRect();
  const x = Math.max(8, Math.min(pt.clientX, window.innerWidth - rect.width - 8));
  const y = Math.max(8, Math.min(pt.clientY, window.innerHeight - rect.height - 8));
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';

  clearTextSelection();
  _armMenuDismiss();
}

// clearTextSelection — drop a selection the browser managed to start.
//
// A long press and right-click are the same gestures used to select text, and on
// some platforms the selection starts BEFORE contextmenu arrives. In the contact
// list user-select: none helps, but on messages it is deliberately allowed
// (copying a message is a basic expectation), so there the highlight did manage
// to appear and stayed hanging under the open menu.
function clearTextSelection() {
  try {
    const sel = window.getSelection();
    if (sel && !sel.isCollapsed) sel.removeAllRanges();
  } catch {}
  // While the menu is open there is nothing to select: the class suppresses
  // user-select across the whole page and is removed on close.
  document.body.classList.add('ctx-open');
}

// ── Closing the menu ───────────────────────────────────────────────────────
//
// On a desktop a click elsewhere is enough. On a phone "clicking elsewhere" is a
// separate deliberate action: the finger is already on the screen, and the
// natural movement to get rid of the menu is to swipe it away. So we also close
// on any scroll or swipe, wherever it starts.
let _menuDismissArmed = false;

function _armMenuDismiss() {
  if (_menuDismissArmed) return;
  _menuDismissArmed = true;
  // A pause before subscribing: the menu opens on a timer while the finger is
  // still on the screen, and its own touchmove/touchend would close it instantly.
  setTimeout(() => {
    if (!_menuDismissArmed) return;
    document.addEventListener('click', closeChatMenu);
    document.addEventListener('touchstart', _dismissOnTouch, { passive: true });
    document.addEventListener('touchmove', _dismissOnSwipe, { passive: true });
    document.addEventListener('wheel', _dismissAlways, { passive: true });
    // capture: a scroll of the contact list does not bubble up to document.
    document.addEventListener('scroll', _dismissAlways, { capture: true, passive: true });
    window.addEventListener('resize', _dismissAlways);
  }, 320);
}

function _disarmMenuDismiss() {
  _menuDismissArmed = false;
  _touchStart = null;
  document.removeEventListener('click', closeChatMenu);
  document.removeEventListener('touchstart', _dismissOnTouch);
  document.removeEventListener('touchmove', _dismissOnSwipe);
  document.removeEventListener('wheel', _dismissAlways);
  document.removeEventListener('scroll', _dismissAlways, { capture: true });
  window.removeEventListener('resize', _dismissAlways);
}

// A touch outside the menu closes it at once — without waiting for click, which
// on a phone arrives with a delay and only if the finger did not move.
let _touchStart = null;
function _dismissOnTouch(e) {
  if (e.target && e.target.closest && e.target.closest('#chat-ctx-menu')) {
    // A touch on the menu itself: we remember the point so a press on an item
    // can be told apart from a swipe started over the menu.
    const t = e.touches && e.touches[0];
    _touchStart = t ? { x: t.clientX, y: t.clientY } : null;
    return;
  }
  _touchStart = null;
  closeChatMenu();
}

// A swipe closes the menu even if it started over it: that is a "put it away"
// movement, not an attempt to press an item. The threshold is essential — the
// finger always shifts a couple of pixels during an ordinary tap, and without it
// the menu would close before the press on an item reached click.
function _dismissOnSwipe(e) {
  if (!_touchStart) return closeChatMenu();
  const t = e.touches && e.touches[0];
  if (!t) return;
  if (Math.abs(t.clientX - _touchStart.x) > 10 || Math.abs(t.clientY - _touchStart.y) > 10) {
    closeChatMenu();
  }
}
function _dismissAlways() { closeChatMenu(); }

function _setMenuItem(menu, id, label) {
  const el = menu.querySelector('#' + id);
  if (!el) return;
  const span = el.querySelector('span');
  if (span) span.textContent = label;
}

function closeChatMenu(e) {
  // A click on the menu item itself is handled by the item, not by this close.
  if (e && e.target && e.target.closest && e.target.closest('#chat-ctx-menu')) return;
  const menu = document.getElementById('chat-ctx-menu');
  if (menu) menu.classList.remove('visible');
  document.body.classList.remove('ctx-open');
  _disarmMenuDismiss();
}

// Menu item actions: close the menu and act on the remembered conversation.
function _ctxChatAct(fn) {
  const peer = ctxChatPeer;
  closeChatMenu();
  if (peer) fn(peer);
}
function cctxPin()     { _ctxChatAct(p => (typeof togglePin === 'function') && togglePin(p)); }
function cctxMute()    { _ctxChatAct(toggleChatMute); }
function cctxArchive() { _ctxChatAct(toggleChatArchive); }
function cctxRead()    { _ctxChatAct(markChatRead); }
function cctxDelete()  { _ctxChatAct(deleteChat); }

// initChatLongPress — a long press opens the same menu.
// The listeners are attached once to the list: rows are recreated on every
// render, so binding to them individually would leak handlers.
function initChatLongPress() {
  const list = document.getElementById('contacts-list');
  if (!list || list._lpBound) return;
  list._lpBound = true;

  let timer = null, startX = 0, startY = 0, firedPeer = null;

  const cancel = () => { clearTimeout(timer); timer = null; };

  list.addEventListener('touchstart', (e) => {
    const row = e.target.closest('.contact-item');
    if (!row || !row.dataset.peer) return;
    const t = e.touches[0];
    startX = t.clientX; startY = t.clientY;
    firedPeer = null;
    cancel();
    timer = setTimeout(() => {
      // Android may have opened the menu through contextmenu already — no need twice.
      if (Date.now() - _ctxOpenedAt < 700) return;
      firedPeer = row.dataset.peer;
      if (navigator.vibrate) { try { navigator.vibrate(12); } catch {} }
      showChatMenu({ clientX: startX, clientY: startY, preventDefault() {} }, firedPeer);
    }, 500);
  }, { passive: true });

  // The finger moved — that is a list scroll, not a long press.
  list.addEventListener('touchmove', (e) => {
    if (!timer) return;
    const t = e.touches[0];
    if (Math.abs(t.clientX - startX) > 10 || Math.abs(t.clientY - startY) > 10) cancel();
  }, { passive: true });

  list.addEventListener('touchend', cancel, { passive: true });
  list.addEventListener('touchcancel', cancel, { passive: true });

  // Lifting the finger after a long press must not also open the chat.
  list.addEventListener('click', (e) => {
    if (!firedPeer) return;
    firedPeer = null;
    e.preventDefault();
    e.stopPropagation();
  }, true);
}
