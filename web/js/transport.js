// Hexeris — WebSocket transport + incoming message router.

// ── Chat init ─────────────────────────────────────────────
// Mobile operating systems kill the socket while a PWA is backgrounded (for
// example while picking a photo), and ws.send into a dead socket loses the
// message silently. Everything important therefore goes through wsSend:
// closed socket -> queue -> flush on open.
let _outbox = [];

// Consecutive failed reconnects, which drive the exponential backoff in
// ws.onclose and reset to 0 on a successful open.
let _reconnectAttempt = 0;
let _reconnectTimer = null;

// When the network returns or the tab is brought forward, reconnect at once
// instead of waiting out the backoff timer. connectWS guards against duplicates.
window.addEventListener('online', () => {
  if (!token || (ws && ws.readyState <= 1)) return;
  _reconnectAttempt = 0;
  connectWS();
});
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState !== 'visible') return;
  if (!token || (ws && ws.readyState <= 1)) return;
  _reconnectAttempt = 0;
  connectWS();
});

function wsSend(obj) {
  const data = JSON.stringify(obj);
  if (ws && ws.readyState === 1) {
    try { ws.send(data); return true; } catch (e) {}
  }
  _outbox.push(data);
  return false;
}

function _flushOutbox() {
  if (!ws || ws.readyState !== 1) return;
  const q = _outbox;
  _outbox = [];
  for (const data of q) {
    try { ws.send(data); } catch (e) { _outbox.push(data); }
  }
  // After the ephemeral control frames, resume guaranteed message delivery.
  flushPendingMessages();
}

// ── Guaranteed message delivery (persistent outbox) ───────────────────────────
// Sending must not depend on a live socket: mobile browsers drop the socket
// in the background and a tab may be reloaded. Every outgoing message is
// therefore written to a local queue first and only then sent. The server is
// idempotent by id, so resending is safe and creates no duplicates. An entry
// leaves the queue on an ACK (sent/delivered/read) or on a final failure.
function _pendingKey() { return 'hc_pending_' + myUsername; }

function _loadPending() {
  try { return JSON.parse(localStorage.getItem(_pendingKey()) || '[]'); }
  catch { return []; }
}
function _savePending(list) {
  try { localStorage.setItem(_pendingKey(), JSON.stringify(list)); } catch {}
}

// Queue a message (or update an existing one by id) and try to send it
// immediately. Returns true when it went out over a live socket.
function queueMessage(envelope) {
  const list = _loadPending();
  const i = list.findIndex(e => e.id === envelope.id);
  const rec = { ...envelope, _queuedAt: Date.now() };
  if (i >= 0) list[i] = rec; else list.push(rec);
  _savePending(list);
  return _trySend(envelope);
}

function _trySend(envelope) {
  if (ws && ws.readyState === 1) {
    try { ws.send(JSON.stringify(envelope)); return true; } catch {}
  }
  return false;
}

// Remove a message from the queue — called from the ACK handler.
function ackPending(id) {
  const list = _loadPending();
  const next = list.filter(e => e.id !== id);
  if (next.length !== list.length) _savePending(next);
}

// Resend everything the server has not confirmed. Called on open and startup.
function flushPendingMessages() {
  if (!ws || ws.readyState !== 1) return;
  for (const envelope of _loadPending()) {
    const { _queuedAt, ...clean } = envelope;
    _trySend(clean);
  }
}

// An unobtrusive "no connection" banner, shown only when the socket is closed
// and messages are waiting, so the user understands why a message shows a
// clock rather than assuming it was lost.
function updateOfflineBanner() {
  let el = document.getElementById('offline-banner');
  const online = ws && ws.readyState === 1;
  const pending = _loadPending().length;
  if (online || !myUsername) { if (el) el.classList.remove('visible'); return; }
  if (!el) {
    el = document.createElement('div');
    el.id = 'offline-banner';
    el.className = 'offline-banner';
    document.body.appendChild(el);
  }
  el.textContent = pending
    ? `No connection — ${pending} message${pending > 1 ? 's' : ''} will send when you're back online`
    : 'Connecting…';
  el.classList.add('visible');
}

function startChat() {
  if (typeof loadLastReadSeq === 'function') loadLastReadSeq();
  if (typeof loadGroups === 'function') loadGroups();
  // Muted and archived conversations before the first render, or the archive
  // flashes in the sidebar and collapses in front of the user.
  if (typeof loadChatPrefs === 'function') loadChatPrefs();
  if (typeof initChatLongPress === 'function') initChatLongPress();
  // Notification permission and automatic push subscription. If permission
  // has not been asked for, ask and subscribe on consent; if it is already
  // granted, quietly ensure a subscription exists. No separate button needed.
  if ('Notification' in window && !(typeof pushOptedOut === 'function' && pushOptedOut())) {
    if (Notification.permission === 'default') {
      Notification.requestPermission().then(() => {
        if (typeof ensurePushSubscribed === 'function') ensurePushSubscribed();
      });
    } else if (typeof ensurePushSubscribed === 'function') {
      ensurePushSubscribed();
    }
  }
  // Clear the previous session's data
  chats = {};
  activePeer = null;
  document.getElementById('contacts-list').innerHTML = '';
  document.getElementById('chat-main').style.display = 'none';
  document.getElementById('chat-empty').style.display = 'flex';

  // Show the loading screen until the key is ready
  const authScreen = document.getElementById('auth-screen');
  const chatScreen = document.getElementById('chat-screen');
  authScreen.style.display = 'none';
  chatScreen.style.display = 'flex';
  document.getElementById('me-label').textContent = myUsername;
  if (typeof loadProfiles === 'function') loadProfiles();
  if (typeof initFeedInsets === 'function') initFeedInsets();
  _hideLoading();

  connectWS();
}

function connectWS() {
  if (!token) return; // never connect without a token
  // Do not open parallel sockets: reconnects come from several sources (the
  // backoff timer, the online event, visibilitychange), and a socket that is
  // already CONNECTING or OPEN needs no second one. logout() closes the
  // socket, so signing back in is not blocked by this guard.
  if (ws && ws.readyState <= 1) return;
  clearTimeout(_reconnectTimer);
  const wsProto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${wsProto}://${SERVER}/ws?token=${token}`);
  ws.onmessage = async (e) => {
    let msg;
    // A malformed frame must not kill the handler (unhandled async rejection).
    try { msg = JSON.parse(e.data); } catch { return; }

    // Server ACK — update the message status
    if (msg.type === 'status') {
      const wasOnline = onlineStatuses[msg.from];
      onlineStatuses[msg.from] = msg.body === 'online';
      const dot = document.getElementById('dot-' + msg.from);
      if (dot) dot.className = (typeof presenceDotClass === 'function') ? presenceDotClass(msg.from) : ('contact-online-dot' + (onlineStatuses[msg.from] ? ' online' : ''));
      if (activePeer === msg.from && typeof showChatHeader === 'function') showChatHeader(msg.from);
      // A member's presence affects "N online" in groups: refresh the list
      // counters and, if a group is open, its header.
      if (wasOnline !== onlineStatuses[msg.from]) {
        if (typeof renderContacts === 'function') renderContacts();
        if (activePeer && activePeer.startsWith('g:') && typeof showChatHeader === 'function') showChatHeader(activePeer);
      }
      return;
    }

    // Peer profile update (name, position, avatar, presence).
    if (msg.type === 'profile') {
      if (typeof handleProfileMsg === 'function') handleProfileMsg(msg);
      return;
    }

    // WebRTC call signaling
    if (['call-offer','call-answer','call-ice','call-end','call-reject'].includes(msg.type)) {
      handleCallMessage(msg);
      return;
    }

    // Reaction
    if (msg.type === 'reaction') {
      const peer = (msg.to && msg.to.startsWith('g:')) ? msg.to : (msg.from === myUsername ? msg.to : msg.from);
      applyReaction(msg.id, msg.emoji, msg.from, peer);
      if (activePeer === peer) updateReactionsBar(peer, msg.id);
      return;
    }

    if (msg.type === 'group-changed') {
      if (typeof handleGroupChanged === 'function') handleGroupChanged();
      return;
    }

    if (msg.type === 'user-deleted') {
      const deleted = msg.username;
      // An administrator deleted me — sign out immediately.
      if (deleted === myUsername) {
        try { clearSession(); } catch {}
        try { if (ws) ws.close(); } catch {}
        location.reload(); // back to the sign-in screen
        return;
      }
      // The peer was deleted and the server already erased the conversation;
      // clear the local cache too, so re-creating the name does not resurrect it.
      try {
        delete chats[deleted];
        localStorage.removeItem('hc_msgs_' + myUsername + '_' + deleted);
        if (typeof lastSeqByPeer !== 'undefined' && lastSeqByPeer) {
          delete lastSeqByPeer[deleted];
          if (typeof saveLastSeq === 'function') saveLastSeq();
        }
      } catch {}
      if (activePeer === deleted) {
        activePeer = null;
        saveActivePeer('');
        document.getElementById('chat-main').style.display = 'none';
        document.getElementById('chat-empty').style.display = '';
        if (typeof showSidebar === 'function') showSidebar();
      }
      if (typeof renderContacts === 'function') renderContacts();
      return;
    }

    if (msg.type === 'typing') {
      if (msg.from !== myUsername) {
        // Direct chat: peer is the sender. Group: peer is the group id.
        const peer = (msg.to && msg.to.startsWith('g:')) ? msg.to : msg.from;
        addTyper(peer, msg.from);
      }
      return;
    }

    if (msg.type === 'deleted') {
      // Mark the message deleted in every chat
      for (const peer of Object.keys(chats)) {
        const m = chats[peer].find(m => m.id === msg.id);
        if (m) {
          m.deleted = true; m.body = '[deleted]';
          try {
            const k = `hc_msgs_${myUsername}_${peer}`;
            const stored = JSON.parse(localStorage.getItem(k) || '[]');
            const sm = stored.find(x => x.id === msg.id);
            if (sm) { sm.deleted = true; sm.body = '[deleted]'; localStorage.setItem(k, JSON.stringify(stored)); }
          } catch {}
          if (activePeer === peer) renderMessages(peer);
          break;
        }
      }
      return;
    }

    if (msg.type === 'edited') {
      // Update the message body in every chat and mark it edited.
      for (const peer of Object.keys(chats)) {
        const m = chats[peer].find(m => m.id === msg.id);
        if (m) {
          m.body = msg.body; m.edited = true;
          try {
            const k = `hc_msgs_${myUsername}_${peer}`;
            const stored = JSON.parse(localStorage.getItem(k) || '[]');
            const sm = stored.find(x => x.id === msg.id);
            if (sm) { sm.body = msg.body; sm.edited = true; localStorage.setItem(k, JSON.stringify(stored)); }
          } catch {}
          if (activePeer === peer) renderMessages(peer);
          break;
        }
      }
      return;
    }

    if (msg.type === 'ack') {
      // Any ACK means the server accepted the message (or rejected it for
      // good), so it leaves the reliable-delivery queue.
      //
      // failed means the server refused it (no such recipient, not a group
      // member). Keeping such a message with a warning forever is pointless:
      // it is removed from the chat and local storage and the user is told.
      if (msg.body === 'failed') {
        for (const peer of Object.keys(chats)) {
          const i = chats[peer].findIndex(m => m.id === msg.id);
          if (i !== -1) {
            chats[peer].splice(i, 1);
            try {
              const k = `hc_msgs_${myUsername}_${peer}`;
              const stored = JSON.parse(localStorage.getItem(k) || '[]').filter(x => x.id !== msg.id);
              localStorage.setItem(k, JSON.stringify(stored));
            } catch (e) {}
            // If this was the only (optimistic) message, do not leave a
            // ghost conversation in the contact list — for example a
            // forward to a non-existent name. Drop the chat entirely.
            if (chats[peer].length === 0 && !peer.startsWith('g:')) {
              delete chats[peer];
              try { localStorage.removeItem(`hc_msgs_${myUsername}_${peer}`); } catch {}
              if (peer === activePeer) {
                activePeer = null;
                document.getElementById('chat-main').style.display = 'none';
                document.getElementById('chat-empty').style.display = '';
                if (typeof showSidebar === 'function') showSidebar();
              }
              if (typeof renderContacts === 'function') renderContacts();
            } else if (peer === activePeer) {
              renderMessages(peer);
            }
            // The server states the reason: guessing it from the peer type
            // reported the wrong one, claiming group membership problems
            // when the message had merely exceeded the length limit.
            const reasons = {
              too_long:   'Message not sent: it is too long (max 4096 characters).',
              not_member: 'Message not sent: you are no longer a member of this group.',
              no_user:    'Message not sent: user "' + peer + '" does not exist.'
            };
            toast(reasons[msg.reason] || 'Message not sent. Please try again.');
            break;
          }
        }
        return;
      }
      const rank = { sending: 0, sent: 1, delivered: 2, read: 3, failed: 1 };
      for (const peer of Object.keys(chats)) {
        const m = chats[peer].find(m => m.id === msg.id);
        if (m) {
          // Never downgrade a status (delivered must not become sent in a race).
          if ((rank[msg.body] ?? 0) >= (rank[m.status] ?? 0)) m.status = msg.body;
          // Take the server's seq and timestamp. Keeping the device's own
          // Date.now() made the sender's send time disagree with what the
          // recipient sees on a phone with a different clock or time zone.
          // Time and order now have one source: the server.
          let _changed = false;
          if (msg.created_at && msg.created_at !== m.created_at) {
            m.created_at = msg.created_at; m.ts = msg.created_at; _changed = true;
          }
          if (msg.seq && msg.seq > 0 && (!m.seq || m.seq === 0)) {
            m.seq = msg.seq; bumpSeq(peer, msg.seq); _changed = true;
          }
          if (_changed) {
            sortChat(peer);
            if (peer === activePeer) renderMessages(peer);
          }
          try {
            const k = `hc_msgs_${myUsername}_${peer}`;
            const stored = JSON.parse(localStorage.getItem(k) || '[]');
            const sm = stored.find(x => x.id === msg.id);
            if (sm) {
              sm.status = m.status;
              if (m.seq) sm.seq = m.seq;
              if (m.created_at) { sm.created_at = m.created_at; sm.ts = m.created_at; }
              localStorage.setItem(k, JSON.stringify(stored));
            }
          } catch {}
          const el = document.getElementById('msg-status-' + msg.id);
          if (el) {
            el.textContent = (m.status === 'delivered' || m.status === 'read') ? '✓✓' : (m.status === 'failed' ? '⚠' : '✓');
            el.className = 'msg-status ' + m.status;
          }
          break;
        }
      }
      return;
    }

    if (msg.type !== 'message') return;

    // Determine the peer and the chat key. For a group message the
    // "conversation" is the group itself rather than the sender.
    const peer = (msg.to && msg.to.startsWith('g:')) ? msg.to : (msg.from === myUsername ? msg.to : msg.from);
    // A message arrived, so this person is no longer typing.
    if (typeof removeTyper === 'function' && msg.from !== myUsername) removeTyper(peer, msg.from);
    // Media messages are not encrypted — the URL travels as-is
    if (msg.media_type) {
      addToChat(peer, { ...msg, media_type: msg.media_type, ts: Date.now() });
      const preview = msg.media_type === 'image' ? '🖼 Image'
        : msg.media_type === 'call' ? ('📞 ' + (String(msg.body).split(':')[0] === 'missed' ? 'Missed call' : 'Call'))
        : '📎 File';
      updateContact(peer, preview);
      // An incoming missed call deserves a visible notification.
      if (msg.media_type === 'call' && msg.from !== myUsername && String(msg.body).split(':')[0] === 'missed') {
        showNotification(peer, '📞 Missed call');
      }
      if (activePeer === peer) { renderMessages(peer); const w = document.getElementById('messages-wrap'); if(w) w.scrollTop = w.scrollHeight; }
      return;
    }

    // The server already decrypted the body, so it is used as-is. The echo of
    // one's own sent message (which syncs other devices and tabs) and a
    // message from the peer are handled identically.
    const body = msg.body;
    const status = msg.from === myUsername ? (msg.status || 'sent') : 'delivered';
    addToChat(peer, { ...msg, body, media_type: msg.media_type,
                      seq: msg.seq, created_at: msg.created_at || Date.now(),
                      ts: msg.created_at || Date.now(), status });
    if (msg.from !== myUsername) updateContact(peer, body);
    if (activePeer === peer) {
      renderMessages(peer);
      const wrap = document.getElementById('messages-wrap');
      if (wrap) wrap.scrollTop = wrap.scrollHeight;
      // The chat is open — mark it read at once.
      if (msg.from !== myUsername && !peer.startsWith('g:') && ws && ws.readyState === 1) {
        ws.send(JSON.stringify({ type: 'read', from: myUsername, to: peer }));
      }
    }
    if (msg.from !== myUsername) showNotification(peer, body);
  };
  ws.onopen = () => {
    _reconnectAttempt = 0; // connected — reset the backoff
    clearInterval(window._pingInterval);
    window._pingInterval = setInterval(() => {
      if (ws && ws.readyState === 1) ws.send(JSON.stringify({ type: 'ping' }));
    }, 25000);
    // Every (re)connect pulls the history missed since the last seq.
    if (!_historyLoading) loadHistory();
    _flushOutbox();
    flushPendingMessages();
    updateOfflineBanner();
  };
  ws.onclose = () => {
    clearInterval(window._pingInterval);
    updateOfflineBanner();
    if (!token) return;
    // Exponential backoff with jitter: after a server restart, thousands of
    // clients on a fixed three-second delay would arrive together (a
    // thundering herd), and against a downed server they would hammer it
    // every three seconds. 3s → 6s → 12s → 24s → 30s, jittered by ±25%.
    const base = Math.min(30000, 3000 * Math.pow(2, _reconnectAttempt));
    _reconnectAttempt = Math.min(_reconnectAttempt + 1, 6);
    const delay = base * (0.75 + Math.random() * 0.5);
    // Before reconnecting, check whether the account was deleted or blocked.
    // A socket carries no HTTP status, so /status answers that cheaply.
    _reconnectTimer = setTimeout(async () => {
      if (!token) return;
      try {
        const r = await fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(myUsername)}`, {
          headers: { 'Authorization': `Bearer ${token}` }
        });
        if (r.status === 401 || r.status === 403 || r.status === 404) {
          // Invalid token or a blocked/deleted account — sign out.
          token = null;
          if (typeof logout === 'function') logout();
          else window.location.reload();
          return;
        }
      } catch {}
      connectWS();
    }, delay);
  };
}

// Reload and decrypt the history with a peer once they come online


// ── Chat management ───────────────────────────────────────

