// Hexeris — per-conversation settings (mute, archive, clear) and the chat
// list's context menu.
//
// The settings live on the server (see server/chatprefs.go) rather than in
// local storage beside pinning: muting must suppress a push before it is
// sent, and a list tidied on a phone has to look the same on a computer. The
// local copy here is only a first-frame cache; without it the list rendered
// with every archived chat and collapsed a moment later.

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
// When the conversation was archived, in milliseconds. 0 means not archived
// or no mark (a row created before the field existed).
function chatArchivedAt(peer) { return chatPrefOf(peer).archived_at || 0; }

// loadChatPrefs: cache first, server next. The server's answer is the truth
// and replaces the cache wholesale, or a setting cleared on another device
// would live on here forever.
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

// _applyClearedBoundary catches up with a clear performed on another device.
//
// After a clear the server stops serving the older messages, but the second
// device already has them locally and its sync cursor is higher, so it would
// never learn about the clear on its own. No dedicated event is needed: the
// boundary is visible in the same settings the device fetches on every start.
//
// Messages without a seq (still being sent) are left alone: they have no
// number yet and would count as older than everything.
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

// setChatPref applies the change locally first and sends it to the server,
// rolling back on failure: silently keeping something the server rejected
// would show the user a setting that does not exist.
//
// localOnly holds fields the interface needs immediately but the server
// computes itself (archived_at) and returns in its response.
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
  // must already sit at the end of the archive. Without it the row briefly
  // dropped to the bottom ("no mark, order unknown") and jumped into place
  // once the answer arrived. The server's value then becomes final.
  await setChatPref(peer, { archived: !wasArchived },
    { archived_at: wasArchived ? 0 : Date.now() });
  // Archiving closes the open conversation: otherwise it stays on screen
  // while its row is gone from the list, and nothing explains what happened.
  if (!wasArchived && activePeer === peer && typeof closeActiveChat === 'function') {
    closeActiveChat();
  }
  if (typeof toast === 'function') {
    toast(wasArchived ? 'Moved out of archive.' : 'Archived.', 'success');
  }
}

// Is the archive section expanded? A function rather than a direct variable:
// ui.js loads before this module, and touching a let binding before its
// initialisation would throw instead of quietly reporting "no archive".
function showArchivedList() { return showArchived; }

function toggleArchivedView() {
  showArchived = !showArchived;
  renderContacts();
}

// markChatRead clears the unread counter without opening the conversation.
function markChatRead(peer) {
  if (typeof markPeerRead !== 'function') return;
  markPeerRead(peer);
  // markPeerRead advances the cursor only over loaded messages, which is
  // enough: unread counts are computed from the same set.
  renderContacts();
  if (typeof updatePageTitle === 'function') updatePageTitle();
  if (ws && ws.readyState === 1 && !peer.startsWith('g:')) {
    ws.send(JSON.stringify({ type: 'read', from: myUsername, to: peer }));
  }
}

// deleteChat is "delete for me". Messages are not erased: the server raises
// a personal visibility boundary, the peer keeps the conversation, and it
// remains available for an internal investigation. The confirmation wording
// must say so, or people believe they erased it for both sides.
async function deleteChat(peer) {
  const name = (typeof displayName === 'function') ? displayName(peer) : peer;
  const ok = (typeof hexConfirm !== 'function') || await hexConfirm(
    'Messages will be removed from your account, on this and your other devices. ' +
    (peer.startsWith('g:') ? 'Other members' : name) + ' will still have their copy.',
    { title: 'Delete chat?', okText: 'Delete', danger: true });
  if (!ok) return;

  try {
    const r = await fetch(`${location.protocol}//${SERVER}/chats/clear`, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ peer })
    });
    if (!r.ok) throw new Error(r.status);
    const res = await r.json();
    chatPrefs[peer] = { ...chatPrefOf(peer), cleared_seq: res.cleared_seq };
    _saveChatPrefsCache();
  } catch {
    if (typeof toast === 'function') toast('Could not delete — check your connection.');
    return;
  }

  // Local traces are removed only after the server succeeds: on a network
  // error the conversation would otherwise vanish from the screen and return
  // at the next sync.
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
  renderContacts();
  if (typeof updatePageTitle === 'function') updatePageTitle();
  if (typeof toast === 'function') toast('Chat deleted.', 'success');
}

// ── Chat list context menu ─────────────────────────────────────────────────

// Right click on a computer, long press on a phone. Android raises
// contextmenu from a long press by itself while iOS does not, so the timer is
// needed separately; _ctxOpenedAt keeps Android from firing both paths and
// opening the menu twice.

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
  // Positioned at the cursor or finger, clamped inside the window so the
  // menu does not run off the bottom edge on a phone.
  const pt = (e && e.touches && e.touches[0]) || e || { clientX: 0, clientY: 0 };
  const rect = menu.getBoundingClientRect();
  const x = Math.max(8, Math.min(pt.clientX, window.innerWidth - rect.width - 8));
  const y = Math.max(8, Math.min(pt.clientY, window.innerHeight - rect.height - 8));
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';

  clearTextSelection();
  _armMenuDismiss();
}

// clearTextSelection drops a selection the browser has already begun.
//
// A long press and a right click are the same gestures that select text, and
// on some platforms selection starts before contextmenu arrives. The contact
// list is covered by user-select: none, but messages deliberately allow
// selection (copying a message is a basic expectation), so the highlight did
// appear there and sat under the open menu.
function clearTextSelection() {
  try {
    const sel = window.getSelection();
    if (sel && !sel.isCollapsed) sel.removeAllRanges();
  } catch {}
  // While the menu is open there is nothing to select: the class disables
  // user-select across the page and is removed on close.
  document.body.classList.add('ctx-open');
}

// ── Closing the menu ───────────────────────────────────────────────────────

// On a computer a click elsewhere is enough. On a phone "clicking elsewhere"
// is a deliberate act, while the natural way to dismiss a menu with a finger
// already on the screen is to swipe. Any scroll or swipe therefore closes it,
// wherever it starts.
let _menuDismissArmed = false;

function _armMenuDismiss() {
  if (_menuDismissArmed) return;
  _menuDismissArmed = true;
  // A pause before subscribing: the menu opens on a timer while the finger
  // is still down, and its own touchmove/touchend would close it instantly.
  setTimeout(() => {
    if (!_menuDismissArmed) return;
    document.addEventListener('click', closeChatMenu);
    document.addEventListener('touchstart', _dismissOnTouch, { passive: true });
    document.addEventListener('touchmove', _dismissOnSwipe, { passive: true });
    document.addEventListener('wheel', _dismissAlways, { passive: true });
    // capture: scrolling the contact list does not bubble to the document.
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

// A touch outside the menu closes it immediately, without waiting for click,
// which on a phone is delayed and only fires if the finger stayed put.
let _touchStart = null;
function _dismissOnTouch(e) {
  if (e.target && e.target.closest && e.target.closest('#chat-ctx-menu')) {
    // A touch on the menu itself records the point, to tell a press on an
    // item from a swipe that started over the menu.
    const t = e.touches && e.touches[0];
    _touchStart = t ? { x: t.clientX, y: t.clientY } : null;
    return;
  }
  _touchStart = null;
  closeChatMenu();
}

// A swipe closes the menu even when it starts over it: that gesture means
// "dismiss", not "press an item". The threshold is essential — a finger
// always drifts a couple of pixels during a normal tap, and without it the
// menu would close before the press reached click.
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
  // A click on a menu item is handled by the item, not by this closer.
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

// initChatLongPress makes a long press open the same menu. The listeners are
// attached once to the list: rows are re-created on every render, so binding
// to them individually would leak handlers.
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
      // Android may have opened the menu through contextmenu already.
      if (Date.now() - _ctxOpenedAt < 700) return;
      firedPeer = row.dataset.peer;
      if (navigator.vibrate) { try { navigator.vibrate(12); } catch {} }
      showChatMenu({ clientX: startX, clientY: startY, preventDefault() {} }, firedPeer);
    }, 500);
  }, { passive: true });

  // The finger moved: this is a list scroll, not a long press.
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
