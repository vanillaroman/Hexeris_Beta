// Hexeris — WebSocket transport + incoming message router.

// ── Chat init ─────────────────────────────────────────────
// Mobile operating systems kill the WS while a PWA is in the background (during
// a photo picker, for instance). ws.send into a dead socket silently loses the
// message — so everything important goes through wsSend: socket closed -> into
// the queue -> flushed on onopen.
let _outbox = [];

// A counter of consecutive failed reconnects — it drives the exponential backoff
// in ws.onclose; reset to 0 on a successful onopen.
let _reconnectAttempt = 0;
let _reconnectTimer = null;

// The network coming back, or the tab/PWA being brought to the front — we do
// not wait out the backoff timer (up to 30 s) but reconnect at once. connectWS
// protects itself against duplicates.
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
  // After the ephemeral control frames we top up guaranteed message delivery.
  flushPendingMessages();
}

// ── Guaranteed message delivery (persistent outbox) ───────────────────────────
// Sending a message must NOT depend on a live socket: mobile browsers tear down
// the WS in the background and a tab can be reloaded. So every outgoing message
// is first put into the localStorage queue and only then sent. The server is
// idempotent (INSERT ... ON CONFLICT(id) — ws.go), so re-sending the same id is
// safe and does not create duplicates. The record is removed from the queue on
// an ACK (sent/delivered/read) or on a final failed.
function _pendingKey() { return 'hc_pending_' + myUsername; }

function _loadPending() {
  try { return JSON.parse(localStorage.getItem(_pendingKey()) || '[]'); }
  catch { return []; }
}
function _savePending(list) {
  try { localStorage.setItem(_pendingKey(), JSON.stringify(list)); } catch {}
}

// Puts a message into the queue (or updates an existing one by id) and tries to
// send it immediately. Returns true if it went into a live socket.
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

// Takes a message off the queue — called from the ACK handler.
function ackPending(id) {
  const list = _loadPending();
  const next = list.filter(e => e.id !== id);
  if (next.length !== list.length) _savePending(next);
}

// Sends everything the server has not confirmed yet. Called on onopen and at start-up.
function flushPendingMessages() {
  if (!ws || ws.readyState !== 1) return;
  for (const envelope of _loadPending()) {
    const { _queuedAt, ...clean } = envelope;
    _trySend(clean);
  }
}

// An unobtrusive "no connection" banner: shown only when the socket is closed
// AND there are unsent messages — so the user understands why a message has an
// "hourglass" instead of thinking it was lost.
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
  // Muted and archived conversations — before the first render of the list,
  // otherwise the archive flashes in the sidebar and collapses before your eyes.
  if (typeof loadChatPrefs === 'function') loadChatPrefs();
  // The menu item label reflects the actual state rather than being static:
  // otherwise the only way to learn whether the second factor is on is to open
  // the window.
  if (typeof refresh2FALabel === 'function') refresh2FALabel();
  if (typeof initChatLongPress === 'function') initChatLongPress();
  // Notification permission + auto-subscription to background push. If the
  // permission has not been asked for yet we ask and, on consent, subscribe
  // right away; if it is already granted we quietly make sure a subscription
  // exists. A separate button is not needed.
  if ('Notification' in window && !(typeof pushOptedOut === 'function' && pushOptedOut())) {
    if (Notification.permission === 'default') {
      Notification.requestPermission().then(() => {
        if (typeof ensurePushSubscribed === 'function') ensurePushSubscribed();
      });
    } else if (typeof ensurePushSubscribed === 'function') {
      ensurePushSubscribed();
    }
  }
  // Clear the previous session data
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
  if (!token) return; // do not connect without a token
  // We do not spawn parallel sockets: a reconnect is initiated by several
  // sources (the backoff timer, the online event, visibilitychange) — if the
  // socket is already CONNECTING/OPEN a second one is not needed. logout()
  // closes the socket, so signing in again is not blocked by this guard.
  if (ws && ws.readyState <= 1) return;
  clearTimeout(_reconnectTimer);
  const wsProto = location.protocol === 'https:' ? 'wss' : 'ws';
  ws = new WebSocket(`${wsProto}://${SERVER}/ws?token=${token}`);
  ws.onmessage = async (e) => {
    let msg;
    // A malformed frame must not bring down the handler (unhandled rejection in async).
    try { msg = JSON.parse(e.data); } catch { return; }

    // An ACK from the server — update the message status
    if (msg.type === 'status') {
      const wasOnline = onlineStatuses[msg.from];
      onlineStatuses[msg.from] = msg.body === 'online';
      const dot = document.getElementById('dot-' + msg.from);
      if (dot) dot.className = (typeof presenceDotClass === 'function') ? presenceDotClass(msg.from) : ('contact-online-dot' + (onlineStatuses[msg.from] ? ' online' : ''));
      if (activePeer === msg.from && typeof showChatHeader === 'function') showChatHeader(msg.from);
      // A member going online or offline affects "N online" in groups: we update
      // the counters in the list and, if a group is open, its header.
      if (wasOnline !== onlineStatuses[msg.from]) {
        if (typeof renderContacts === 'function') renderContacts();
        if (activePeer && activePeer.startsWith('g:') && typeof showChatHeader === 'function') showChatHeader(activePeer);
      }
      return;
    }

    // A peer profile update (name/title/avatar/presence).
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
        location.reload(); // takes us back to the sign-in screen
        return;
      }
      // The peer was deleted: the server has already wiped the conversation — we
      // clear the local cache too, so that old history does not resurface if the
      // same username is created again. We also remember the fact itself: there
      // is no need to ask for their presence and profile, the answer is known.
      if (typeof markPeerGone === 'function') markPeerGone(deleted);
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
        // A direct chat — peer = the sender; a group — peer = g:<id>.
        const peer = (msg.to && msg.to.startsWith('g:')) ? msg.to : msg.from;
        addTyper(peer, msg.from);
      }
      return;
    }

    if (msg.type === 'deleted') {
      // Mark the message deleted in all chats
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
      // Update the message body in all chats + mark it as edited.
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
      // Any ACK means the server accepted the message (or finally rejected it) —
      // we take it off the guaranteed-delivery queue.
      if (typeof ackPending === 'function') ackPending(msg.id);
      // failed = the server rejected it (non-existent recipient / not a group
      // member). Keeping such a message with a "⚠" forever is pointless — we
      // remove it from the chat and localStorage and tell the user plainly.
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
            // If this was the only (optimistic) message — do not leave an empty
            // "ghost conversation" in the contact list (e.g. a forward to a
            // non-existent username). We delete the chat entirely.
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
            // The reason is reported by the server (msg.reason) — the client
            // used to guess it from the peer type and lied about group
            // membership when the message had simply exceeded the length limit.
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
          // Do not downgrade the status (delivered must not become sent through a race).
          if ((rank[msg.body] ?? 0) >= (rank[m.status] ?? 0)) m.status = msg.body;
          // We accept the server's seq AND time. Our own message used to keep
          // the device's local Date.now() — on a phone with a different clock or
          // time zone the send time diverged from what the recipient sees. Now
          // there is one source of time and order: the server.
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

    // Determine the peer and get the key.
    // For a group message the "conversation" is the group itself, not the sender.
    const peer = (msg.to && msg.to.startsWith('g:')) ? msg.to : (msg.from === myUsername ? msg.to : msg.from);
    // A message arrived — so this person is no longer "typing".
    if (typeof removeTyper === 'function' && msg.from !== myUsername) removeTyper(peer, msg.from);
    // And so they exist: the "deleted" mark is cleared at once. Otherwise a
    // username registered again within a day would stay without presence and
    // profile — silently and until the mark expired.
    if (msg.from !== myUsername && typeof unmarkPeerGone === 'function') unmarkPeerGone(msg.from);
    // Media messages are not encrypted — the URL is passed through as is
    if (msg.media_type) {
      addToChat(peer, { ...msg, media_type: msg.media_type, ts: Date.now() });
      const preview = msg.media_type === 'image' ? '🖼 Image'
        : msg.media_type === 'call' ? ('📞 ' + (String(msg.body).split(':')[0] === 'missed' ? 'Missed call' : 'Call'))
        : '📎 File';
      updateContact(peer, preview);
      // An incoming "missed call" — an immediately visible notification for the recipient.
      if (msg.media_type === 'call' && msg.from !== myUsername && String(msg.body).split(':')[0] === 'missed') {
        showNotification(peer, '📞 Missed call');
      }
      if (activePeer === peer) { renderMessages(peer); const w = document.getElementById('messages-wrap'); if(w) w.scrollTop = w.scrollHeight; }
      return;
    }

    // The server has already decrypted the body — we use it as is. An echo of
    // our own sent message (to sync other devices/tabs) and a message from the
    // peer are handled the same way.
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
      // The chat is open — mark it read immediately.
      if (msg.from !== myUsername && !peer.startsWith('g:') && ws && ws.readyState === 1) {
        ws.send(JSON.stringify({ type: 'read', from: myUsername, to: peer }));
      }
    }
    if (msg.from !== myUsername) {
      showNotification(peer, body);
      // A screen reader learns about a message only from here: the client used
      // to have no aria-live region at all, and an arriving message was not
      // announced in any way. Coalescing the stream is announceMessage's job.
      if (typeof announceMessage === 'function') {
        announceMessage(typeof displayName === 'function' ? displayName(msg.from) : msg.from, body);
      }
    }
  };
  ws.onopen = () => {
    _reconnectAttempt = 0; // connected — reset the backoff
    clearInterval(window._pingInterval);
    window._pingInterval = setInterval(() => {
      if (ws && ws.readyState === 1) ws.send(JSON.stringify({ type: 'ping' }));
    }, 25000);
    // On every (re)connection we pull the missed history by seq.
    if (!_historyLoading) loadHistory();
    // And we check the configuration. A server restart with a new flag is
    // visible to the client precisely as a socket break and recovery — there is
    // no other signal, and without this line a capability enabled by an
    // administrator would only reach the person after a page reload.
    if (typeof refreshAppConfig === 'function') refreshAppConfig(true);
    _flushOutbox();
    flushPendingMessages();
    updateOfflineBanner();
  };
  ws.onclose = () => {
    clearInterval(window._pingInterval);
    updateOfflineBanner();
    if (!token) return;
    // The FIRST attempt is immediate. The most common disconnect in production
    // is not "the network went down" but a server restart during a deploy: by
    // the time the browser notices the break the server is usually already up.
    // The previous code still waited at least 3 seconds, and EVERY client paid
    // those 3 seconds on EVERY deploy — exactly the "it lags until you reload"
    // that started this investigation.
    //
    // After that — exponential backoff: a server that is down must not be
    // hammered. 0 → 1s → 3s → 6s → 12s → 24s → 30s (ceiling).
    const attempt = _reconnectAttempt;
    _reconnectAttempt = Math.min(_reconnectAttempt + 1, 6);
    // Jitter is needed on the first attempt too: a thousand clients woken by one
    // restart must not arrive in the same millisecond. But 300 ms of spread is
    // enough to smear the spike, and it is imperceptible to a person.
    const delay = attempt === 0
      ? Math.random() * 300
      : Math.min(30000, 1500 * Math.pow(2, attempt - 1)) * (0.75 + Math.random() * 0.5);

    _reconnectTimer = setTimeout(async () => {
      if (!token) return;
      // A check for whether the account has been deleted or blocked (WS does not
      // return an HTTP code). On the FIRST attempt we skip it: that is an extra
      // round-trip before the connection that matters most for speed, and a
      // blocked account will be rejected by the socket handshake anyway — the
      // check then runs on the next round, that is, a second later.
      if (attempt > 0) {
        try {
          const r = await fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(myUsername)}`, {
            headers: { 'Authorization': `Bearer ${token}` }
          });
          if (r.status === 401 || r.status === 403 || r.status === 404) {
            // The token is invalid or the account is blocked/deleted — sign out.
            token = null;
            if (typeof logout === 'function') logout();
            else window.location.reload();
            return;
          }
        } catch {}
      }
      connectWS();
    }, delay);
  };
}

// Reloads and decrypts the history with a specific peer when they come online


// ── Chat management ───────────────────────────────────────


// isQueued — whether a message is still in the unconfirmed queue. Reads the
// same list as flushPendingMessages: the single source of truth about what the
// server has not confirmed yet.
function isQueued(id) {
  try { return _loadPending().some(e => e.id === id); } catch { return false; }
}

// pendingMessageIds — the whole set of unconfirmed ids at once. Rendering the
// feed asks for it ONCE per pass: isQueued in a loop would mean reading
// localStorage and parsing JSON for every message.
function pendingMessageIds() {
  try { return new Set(_loadPending().map(e => e.id)); } catch { return new Set(); }
}
