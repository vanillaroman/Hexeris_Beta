// Hexeris — context menu, reactions, reply/forward, logout.

// ── Context Menu ─────────────────────────────────────────
let ctxMsgId = null;
let ctxIsOwn = false;

function showCtxMenu(e, msgId, isOwn) {
  e.preventDefault();
  ctxMsgId = msgId;
  ctxIsOwn = isOwn;
  const menu = document.getElementById('ctx-menu');
  document.getElementById('ctx-delete-item').style.display = isOwn ? 'flex' : 'none';
  // Edit: own text messages only (not media, not deleted).
  const _em = (chats[activePeer] || []).find(x => x.id === msgId);
  document.getElementById('ctx-edit-item').style.display = (isOwn && _em && !_em.media_type && !_em.deleted) ? 'flex' : 'none';
  // Download applies only to media and files, whose body points at /files/.
  const _isFile = _em && _em.media_type && _em.body && _em.body.startsWith('/files/');
  document.getElementById('ctx-download-item').style.display = _isFile ? 'flex' : 'none';
  menu.classList.add('visible');
  // Position at the cursor, clamped inside the viewport.
  const x = Math.min(e.clientX, window.innerWidth - 190);
  const y = Math.min(e.clientY, window.innerHeight - 240);
  menu.style.left = x + 'px';
  menu.style.top = y + 'px';
  // A long press and a right click are the same gestures that select text,
  // and the browser starts selecting before contextmenu arrives. Message
  // bodies deliberately allow selection (copying a message is a basic
  // expectation), so the highlight appeared and sat under the open menu.
  if (typeof clearTextSelection === 'function') clearTextSelection();
  // Close on outside click
  setTimeout(() => document.addEventListener('click', closeCtxMenu, { once: true }), 0);
}

function closeCtxMenu(e) {
  // A click on a menu item (say "React") must not be closed by this handler:
  // the item decides what to do, and React opens the emoji picker. With
  // delegation, data-stop calls stopPropagation at the document level, which
  // does not affect this sibling listener, so the target is checked
  // explicitly — otherwise the picker an item just opened collapsed at once.
  if (e && e.target.closest && e.target.closest('#ctx-menu')) return;
  document.getElementById('ctx-menu').classList.remove('visible');
  // Lift the selection block only when no chat menu is open: both set the
  // same class, and closing one must not unblock the other.
  const chatMenu = document.getElementById('chat-ctx-menu');
  if (!chatMenu || !chatMenu.classList.contains('visible')) {
    document.body.classList.remove('ctx-open');
  }
  closeEmojiPicker(e);
}

function ctxReact() {
  document.getElementById('ctx-menu').classList.remove('visible');
  // Show emoji picker near the message
  const bubble = document.querySelector(`[data-id="${ctxMsgId}"]`);
  if (!bubble) return;
  const rect = bubble.getBoundingClientRect();
  const picker = document.getElementById('emoji-picker');
  picker.classList.add('visible');
  picker.style.left = Math.min(rect.left, window.innerWidth - 210) + 'px';
  picker.style.top = Math.max(rect.top - 60, 10) + 'px';
  setTimeout(() => document.addEventListener('click', closeEmojiPicker, { once: true }), 100);
}

function closeEmojiPicker(e) {
  // A click on an emoji does not close the picker here — addReaction closes
  // it after sending. Otherwise the same click that picks an emoji (or the
  // click on "React" that opened it) would collapse it immediately.
  if (e && e.target.closest && e.target.closest('#emoji-picker')) return;
  document.getElementById('emoji-picker').classList.remove('visible');
}

function ctxReply() {
  closeCtxMenu();
  const msgs = chats[activePeer] || [];
  const m = msgs.find(x => x.id === ctxMsgId);
  if (!m) return;
  replyToMsg = ctxMsgId;
  const text = m.body.startsWith('/files/') ? 'File' : m.body.substring(0, 60);
  const bar = document.getElementById('reply-bar-input');
  const barText = document.getElementById('reply-bar-input-text');
  barText.innerHTML = `<strong style="color:var(--accent)">${escHtml(m.from)}</strong>: ${escHtml(text)}`;
  bar.style.display = 'flex';
  // Focusing the textarea opens the keyboard on mobile and desktop alike
  const _ta = document.getElementById('msg-textarea');
  if (_ta) {
    _ta.focus();
    // On iOS focus() without a user gesture does not open the keyboard,
    // but this runs from a tap or click, so the gesture is there.
    _ta.setSelectionRange(_ta.value.length, _ta.value.length);
  }
}

function cancelReply() {
  replyToMsg = null;
  document.getElementById('reply-bar-input').style.display = 'none';
}

// ── Edit ──────────────────────────────────────────────────
let editingMsg = null; // id of the message currently being edited (own text only)
function ctxEdit() {
  closeCtxMenu();
  const m = (chats[activePeer] || []).find(x => x.id === ctxMsgId);
  if (!m || m.media_type || m.deleted) return;
  cancelReply(); // edit and reply are mutually exclusive
  editingMsg = ctxMsgId;
  const ta = document.getElementById('msg-textarea');
  ta.value = m.body;
  autoResize(ta);
  document.getElementById('edit-bar-input').style.display = 'flex';
  ta.focus();
  ta.setSelectionRange(ta.value.length, ta.value.length);
}
function cancelEdit() {
  editingMsg = null;
  document.getElementById('edit-bar-input').style.display = 'none';
  const ta = document.getElementById('msg-textarea');
  if (ta) { ta.value = ''; autoResize(ta); }
}

// ArrowUp on an empty composer edits your most recent editable message.
function editLastOwnMessage() {
  const msgs = chats[activePeer] || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const m = msgs[i];
    if (m.from === myUsername && !m.media_type && !m.deleted && m.id) {
      ctxMsgId = m.id;
      ctxEdit();
      return true;
    }
  }
  return false;
}

function ctxForward() {
  closeCtxMenu();
  const m = (chats[activePeer] || []).find(x => x.id === ctxMsgId);
  if (!m) return;
  forwardMsg = m;
  openForward();
}

// Forward modal: a manual username field on top, with existing contacts and
// groups below for one-click sending.
function openForward() {
  const ov = document.getElementById('forward-overlay');
  const list = document.getElementById('forward-list');
  const inp = document.getElementById('forward-input');
  if (!ov || !list) return;
  if (inp) inp.value = '';
  const peers = Object.keys(chats).filter(p =>
    !p.startsWith('g:') || (typeof groups !== 'undefined' && groups[p]));
  list.innerHTML = '';
  if (!peers.length) {
    list.innerHTML = '<div class="fwd-empty">No contacts yet — type a username above.</div>';
  }
  for (const peer of peers) {
    const isGrp = peer.startsWith('g:');
    const img = (!isGrp && typeof avatarImg === 'function') ? avatarImg(peer) : '';
    const row = document.createElement('div');
    row.className = 'fwd-item';
    row.innerHTML =
      `<div class="contact-avatar ${avatarClass(peer)}${img ? ' has-img' : ''}${isGrp ? ' is-group' : ''}">` +
      `${img || escHtml(displayName(peer)[0].toUpperCase())}</div><span>${escHtml(displayName(peer))}</span>`;
    row.onclick = () => doForward(peer);
    list.appendChild(row);
  }
  ov.classList.add('open');
  // Deliberately not focused: on a phone that raises the keyboard at once,
  // while most people pick from the list rather than type a name.
}

function closeForward() {
  const ov = document.getElementById('forward-overlay');
  if (ov) ov.classList.remove('open');
}

function forwardTyped() {
  const inp = document.getElementById('forward-input');
  doForward(((inp && inp.value) || '').trim());
}

async function doForward(peer) {
  if (!forwardMsg || !peer || peer === myUsername) { closeForward(); return; }
  // A hand-typed name that is not yet a contact is checked for existence,
  // or a ghost conversation is created.
  if (!chats[peer] && !peer.startsWith('g:')) {
    try {
      const r = await fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(peer)}`,
        { headers: { 'Authorization': `Bearer ${token}` } });
      if (r.status === 404) { toast('User "' + peer + '" does not exist.'); return; }
    } catch {}
  }
  forwardMessage(peer, forwardMsg);
  closeForward();
}

async function forwardMessage(toPeer, origMsg) {
  const msgID = Date.now() + '-' + Math.floor(Math.random() * 99999);

  const msg = { type: 'message', id: msgID, from: myUsername, to: toPeer,
                body: origMsg.body, media_type: origMsg.media_type, forwarded: true };
  // Through the persistent queue, so a forward survives a dropped connection.
  queueMessage(msg);
  if (!chats[toPeer]) chats[toPeer] = [];
  addToChat(toPeer, { ...msg, status: 'sending', ts: Date.now(), forwarded: true });
  if (activePeer === toPeer) renderMessages(toPeer);
  updateOfflineBanner();
  toast('Forwarded to ' + toPeer, 'success');
}

function ctxCopy() {
  closeCtxMenu();
  const msgs = chats[activePeer] || [];
  const m = msgs.find(x => x.id === ctxMsgId);
  if (m && m.body) navigator.clipboard.writeText(m.body).catch(() => {});
}

function ctxDownload() {
  closeCtxMenu();
  const m = (chats[activePeer] || []).find(x => x.id === ctxMsgId);
  if (m && m.media_type && m.body && m.body.startsWith('/files/')) downloadFile(m.body);
}

// downloadFile fetches from /files/ through a blob: media is served inline,
// so an ordinary click does not save it. The bytes are pulled with the bearer
// token and saved under the original name.
async function downloadFile(body) {
  const fname = fileName(body);   // the real name from the #fragment (or the on-disk name)
  const url = fileUrl(body);      // the URL without the fragment
  try {
    const res = await fetch(`${location.protocol}//${SERVER}${url}`, {
      headers: { 'Authorization': 'Bearer ' + token },
    });
    if (!res.ok) throw new Error(res.status);
    const blob = await res.blob();
    const obj = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = obj;
    a.download = fname;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(obj), 1000);
  } catch (e) {
    if (typeof toast === 'function') toast('Download failed');
  }
}

function ctxDelete() {
  closeCtxMenu();
  if (ctxIsOwn) deleteMessage(ctxMsgId);
}

// ── Reactions ─────────────────────────────────────────────
let replyToMsg = null;
let forwardMsg = null;

function addReaction(emoji) {
  closeEmojiPicker();
  toggleReaction(ctxMsgId, emoji);
}

function toggleReaction(msgId, emoji) {
  if (!activePeer || !ws) return;
  ws.send(JSON.stringify({ type: 'reaction', to: activePeer, id: msgId, emoji: emoji, from: myUsername }));
  applyReaction(msgId, emoji, myUsername, activePeer);
  // Targeted: a full renderMessages per reaction rebuilt the entire feed.
  updateReactionsBar(activePeer, msgId);
}

function applyReaction(msgId, emoji, fromUser, peer) {
  const chatPeer = peer || activePeer;
  if (!chatPeer) return;
  const msgs = chats[chatPeer] || [];
  const m = msgs.find(x => x.id === msgId);
  if (!m || !emoji) return;
  if (!m.reactions) m.reactions = {};
  if (!m.reactions[fromUser]) m.reactions[fromUser] = [];
  const idx = m.reactions[fromUser].indexOf(emoji);
  if (idx >= 0) {
    m.reactions[fromUser].splice(idx, 1);
  } else {
    m.reactions[fromUser].push(emoji);
  }
}

// Update sendMessage to support reply
let _origSendMessage = null;

function logout() {
  // Unsubscribe this device from push before clearing the session, or it
  // would keep receiving the account's notifications after signing out,
  // incoming calls included. The token is captured before the first await.
  try { if (typeof disableNotifications === 'function') disableNotifications(true); } catch {}
  // Clear the HttpOnly auth cookie on the server; JS cannot delete it.
  fetch(`${location.protocol}//${SERVER}/api/session-cookie`, { method: 'DELETE' }).catch(() => {});
  clearSession();
  token = null; myUsername = null; activePeer = null; chats = {}; lastSeqByPeer = {}; Object.keys(scrollPositions).forEach(k => delete scrollPositions[k]);
  // Conversation settings are session state too: without a reset the next
  // person on this device would see someone else's archive and mutes.
  if (typeof chatPrefs !== 'undefined') { chatPrefs = {}; showArchived = false; }
  clearInterval(window._pingInterval);
  clearInterval(window._syncInterval);
  if (ws) ws.close();
  document.getElementById('chat-screen').style.display = 'none';
  // Through the shared entry point: the config is applied, so it appears instantly.
  if (typeof showAuthScreen === 'function') showAuthScreen();
  else document.getElementById('auth-screen').style.display = 'flex';
  document.getElementById('auth-username').value = '';
  document.getElementById('auth-password').value = '';
}
