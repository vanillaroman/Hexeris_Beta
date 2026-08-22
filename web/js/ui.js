// Hexeris — chat UI: contacts, messages, send/file, rendering.


function isMobile() { return window.innerWidth <= 600; }

function showSidebar() {
  document.querySelector('.sidebar').classList.remove('hidden');
  document.querySelector('.chat-area').classList.add('hidden');
}

function showChat() {
  if (isMobile()) {
    document.querySelector('.sidebar').classList.add('hidden');
    document.querySelector('.chat-area').classList.remove('hidden');
  }
}

function openChat(peer) {
  if (!peer) peer = document.getElementById('new-chat-user').value.trim();
  if (!peer) return;
  document.getElementById('new-chat-user').value = '';
  // A new direct conversation is only created with an existing user: a typo
  // in a username used to spawn a permanent ghost chat in the sidebar.
  if (!chats[peer] && !peer.startsWith('g:')) {
    fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(peer)}`, {
      headers: { 'Authorization': `Bearer ${token}` }
    }).then(r => {
      if (r.status === 404) { toast('User "' + peer + '" does not exist.'); return; }
      if (!r.ok && r.status !== 404) return; // network/server — do not fail silently
      chats[peer] = [];
      _openChatInner(peer);
    }).catch(() => { chats[peer] = []; _openChatInner(peer); });
    return;
  }
  if (!chats[peer]) chats[peer] = [];
  _openChatInner(peer);
}

function _openChatInner(peer) {

  // Remember the previous chat's scroll position
  if (activePeer) {
    const wrap = document.getElementById('messages-wrap');
    if (wrap) scrollPositions[activePeer] = wrap.scrollTop;
  }

  activePeer = peer;
  saveActivePeer(peer);
  if (typeof closeChatSearch === 'function') closeChatSearch();
  // Reply is scoped to a chat: leaving one clears an unfinished reply, or the
  // preview from chat A leaks into chat B and points at someone else's
  // message. Editing is cleared only when active, so an ordinary chat switch
  // does not wipe a draft in the composer.
  if (typeof cancelReply === 'function') cancelReply();
  if (typeof editingMsg !== 'undefined' && editingMsg && typeof cancelEdit === 'function') cancelEdit();

  document.querySelectorAll('.contact-item').forEach(el => el.classList.remove('active'));
  const el = document.getElementById('contact-' + peer);
  if (el) el.classList.add('active');

  markPeerRead(peer);
  renderContacts();
  showChatHeader(peer);
  renderMessages(peer);
  if (typeof renderTyping === 'function') renderTyping(peer);
  updatePageTitle();

  // Restore the scroll position, or scroll to the end
  const wrap = document.getElementById('messages-wrap');
  if (wrap) {
    if (scrollPositions[peer] !== undefined) {
      wrap.scrollTop = scrollPositions[peer];
      // Stick to the bottom only if the saved position was at the bottom.
      wrap._stickBottom = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 150;
    } else {
      wrap.scrollTop = wrap.scrollHeight;
      // Entering at the end of a conversation stays pinned to the bottom
      // while photos and videos load, or Safari jumps the feed upwards.
      wrap._stickBottom = true;
    }
  }

  // Mark the whole conversation read on open
  if (ws && ws.readyState === 1) {
    if (!peer.startsWith('g:')) ws.send(JSON.stringify({ type: 'read', from: myUsername, to: peer }));
  }

  // The peer's profile: the contact may have just been opened and not be in
  // the preloaded peer list yet.
  if (!peer.startsWith('g:') && typeof fetchPeerProfile === 'function') fetchPeerProfile(peer);

  // Ask for the peer's current online status when opening a chat. Not for
  // groups: the asynchronous answer overwrote the "N members" header with
  // "offline", since a group is not a user as far as /status is concerned.
  if (!peer.startsWith('g:'))
  fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(peer)}`, {
    headers: { 'Authorization': `Bearer ${token}` }
  }).then(r => r.ok ? r.json() : null).then(d => {
    if (d) {
      onlineStatuses[peer] = d.online;
      const dot = document.getElementById('dot-' + peer);
      if (dot) dot.className = (typeof presenceDotClass === 'function') ? presenceDotClass(peer) : ('contact-online-dot' + (d.online ? ' online' : ''));
      if (activePeer === peer) showChatHeader(peer);
    }
  }).catch(() => {});

  showChat();
  if (!('ontouchstart' in window)) { const _ta = document.getElementById('msg-textarea'); _ta.focus(); _ta.setSelectionRange(0,0); _ta.scrollTop = 0; }
}

function showChatHeader(peer) {
  document.getElementById('chat-empty').style.display = 'none';
  const main = document.getElementById('chat-main');
  main.style.display = 'flex';

  const av = document.getElementById('chat-header-avatar');
  const isGrp = peer.startsWith('g:');
  const hdrImg = (!isGrp && typeof avatarImg === 'function') ? avatarImg(peer) : '';
  if (hdrImg) {
    av.innerHTML = hdrImg;
    av.className = 'chat-header-avatar ' + avatarClass(peer) + ' has-img';
  } else {
    av.textContent = displayName(peer)[0].toUpperCase();
    av.className = 'chat-header-avatar ' + avatarClass(peer) + (isGrp ? ' is-group' : '');
  }
  document.getElementById('chat-header-name').textContent = displayName(peer);
  const statusEl = document.getElementById('chat-header-status');
  const nameEl = document.getElementById('chat-header-name');
  if (isGrp) {
    const n = groups[peer] ? Object.keys(groups[peer].members).length : 0;
    const on = groupOnlineCount(peer);
    // Only "Manage" is clickable, not the whole header.
    statusEl.innerHTML = n + (n === 1 ? ' member' : ' members') +
      (on ? ' · <span style="color:var(--online)">' + on + ' online</span>' : '') +
      ' · <span class="gp-manage" data-act="openGroupPanel" data-stop title="Group members &amp; settings">' +
      '<svg viewBox="0 0 24 24"><path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87"/></svg>Manage</span>';
    statusEl.style.color = 'var(--muted)';
    for (const el of [statusEl, nameEl, av]) {
      el.style.cursor = ''; el.onclick = null; el.title = '';
    }
  } else {
    const pl = (typeof presenceLabel === 'function')
      ? presenceLabel(peer)
      : { text: onlineStatuses[peer] ? 'online' : 'offline', color: onlineStatuses[peer] ? 'var(--online)' : 'var(--muted)' };
    const pos = (typeof profiles !== 'undefined' && profiles[peer] && profiles[peer].position) || '';
    statusEl.innerHTML = (pos ? escHtml(pos) + ' · ' : '') +
      '<span style="color:' + pl.color + '">' + escHtml(pl.text) + '</span>';
    statusEl.style.color = 'var(--muted)';
    for (const el of [statusEl, nameEl, av]) {
      el.style.cursor = '';
      el.onclick = null;
      el.title = '';
    }
  }
  // Group calls are not implemented; the buttons are hidden so nobody hits
  // the broken "offer to a group" path.
  document.querySelectorAll('.call-start-btn').forEach(b => b.style.display = isGrp ? 'none' : '');
  // Group management opens from "Manage" next to the member list. On mobile
  // the status row holding it is hidden for space, so groups also get this
  // header icon, which stays reachable in portrait orientation.
  const gib = document.getElementById('group-info-btn');
  if (gib) gib.style.display = isGrp ? 'flex' : 'none';
}


// ── Unread counters ──────────────────────────────────────
// lastReadSeq[peer] is the seq of the last message the user has seen (by
// opening the chat). Anything above it from someone else counts as unread.
let lastReadSeq = {};

function loadLastReadSeq() {
  try {
    lastReadSeq = JSON.parse(localStorage.getItem('hc_lastread_' + myUsername) || '{}');
  } catch { lastReadSeq = {}; }
}

function saveLastReadSeq() {
  try { localStorage.setItem('hc_lastread_' + myUsername, JSON.stringify(lastReadSeq)); } catch {}
}

function markPeerRead(peer) {
  const msgs = chats[peer] || [];
  let maxSeq = lastReadSeq[peer] || 0;
  for (const m of msgs) if (m.seq > maxSeq) maxSeq = m.seq;
  if (maxSeq > (lastReadSeq[peer] || 0)) {
    lastReadSeq[peer] = maxSeq;
    saveLastReadSeq();
  }
}

function countUnread(peer) {
  const cutoff = lastReadSeq[peer] || 0;
  let n = 0;
  for (const m of (chats[peer] || [])) {
    if (m.seq > cutoff && m.from !== myUsername && !m.deleted) n++;
  }
  return n;
}

function updatePageTitle() {
  const total = Object.keys(chats).reduce((s, p) => s + countUnread(p), 0);
  const base = window.APP_NAME || 'Hexeris';
  document.title = total > 0 ? '(' + total + ') ' + base : base;
}

// ── Pinning chats and groups to the top. A per-device preference, kept
// locally so it needs no server or schema. ──
const PINNED_KEY = 'hc_pinned';
function pinnedSet() {
  try { return new Set(JSON.parse(localStorage.getItem(PINNED_KEY) || '[]')); }
  catch { return new Set(); }
}
function togglePin(peer) {
  const s = pinnedSet();
  s.has(peer) ? s.delete(peer) : s.add(peer);
  localStorage.setItem(PINNED_KEY, JSON.stringify([...s]));
  renderContacts();
}
const PIN_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="12" y1="17" x2="12" y2="22"/><path d="M9 4h6l-1 6 3 3H7l3-3-1-6z"/></svg>';
// A struck-through bell marks a muted conversation — the same glyph as the
// menu item, so "I pressed Mute and got this" reads immediately.
const MUTE_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M13.73 21a2 2 0 0 1-3.46 0"/><path d="M18.63 13A17.9 17.9 0 0 1 18 8"/><path d="M6.26 6.26A5.86 5.86 0 0 0 6 8c0 7-3 9-3 9h14"/><path d="M18 8a6 6 0 0 0-9.33-5"/><line x1="1" y1="1" x2="23" y2="23"/></svg>';

// closeActiveChat leaves a conversation without opening another. Needed when
// the open chat disappears from the list (archived, deleted): without it the
// chat stayed on screen while its sidebar row was already gone.
function closeActiveChat() {
  activePeer = null;
  saveActivePeer('');
  document.querySelectorAll('.contact-item').forEach(el => el.classList.remove('active'));
  const main = document.getElementById('chat-main');
  if (main) main.style.display = 'none';
  const empty = document.getElementById('chat-empty');
  if (empty) empty.style.display = 'flex';
  if (isMobile()) showSidebar();
}

// Is the archive section expanded? Checked through typeof rather than
// directly: chatmenu.js loads after this file, so before it runs the variable
// does not exist yet — the list must still render rather than throw.
function _archiveOpen() {
  return (typeof showArchivedList === 'function') && showArchivedList();
}

function renderContacts() {
  const list = document.getElementById('contacts-list');
  // Only groups we belong to are shown: messages from a group we left stay
  // in local storage, and without this filter it lingered in the sidebar as
  // a ghost with no members and no way out.
  const pins = pinnedSet();
  const archived = (p) => (typeof chatIsArchived === 'function') && chatIsArchived(p);
  const visible = Object.keys(chats).filter(p =>
    !p.startsWith('g:') || (typeof groups !== 'undefined' && groups[p])
  );
  // The archive is a section rather than a "show everything" filter: mixing
  // archived conversations back in restores exactly the list the archive was
  // created to fix.
  const archivedPeers = visible.filter(archived);

  // Recency drives the active list. lastActivity is computed once per
  // conversation rather than inside the comparator, where it was recomputed
  // for every comparison — O(n log n) times instead of n.
  const lastActivity = (p) => {
    const m = chats[p];
    if (!m || !m.length) return 0;
    const last = m[m.length - 1];
    return last.ts || parseInt(last.id) || 0;
  };
  const archivedAt = (p) => (typeof chatArchivedAt === 'function') ? chatArchivedAt(p) : 0;

  const peers = visible.filter(p => !archived(p) || _archiveOpen()).sort((a, b) => {
    // The archive always sits below active chats, even when expanded: it
    // hangs under the "Archived" row rather than interleaving with it.
    const aa = archived(a), ab = archived(b);
    if (aa !== ab) return aa ? 1 : -1;
    if (aa) {
      // Inside the archive, by archiving time with the most recent on top.
      // Sorting archived conversations by message recency is pointless:
      // these are exactly the chats nobody awaits, and someone else's reply
      // would reshuffle the section. Rows without a mark (archived before
      // the field existed) sink rather than rise, since 0 would otherwise
      // read as "archived just now".
      const ta = archivedAt(a) || 0, tb = archivedAt(b) || 0;
      if (ta !== tb) return tb - ta;
      return a < b ? -1 : 1;            // equal times: by name, for stability
    }
    const pa = pins.has(a), pb = pins.has(b);
    if (pa !== pb) return pa ? -1 : 1;   // pinned always on top
    return lastActivity(b) - lastActivity(a);
  });
  list.innerHTML = '';

  // The "Archived (N)" row is the section's entry point, shown only when the
  // archive holds something: an empty item is noise.
  if (archivedPeers.length) {
    const arcUnread = archivedPeers.reduce((s, p) => s + countUnread(p), 0);
    const row = document.createElement('div');
    row.className = 'archive-row' + (_archiveOpen() ? ' open' : '');
    row.setAttribute('role', 'button');
    row.tabIndex = 0;
    row.setAttribute('aria-expanded', _archiveOpen() ? 'true' : 'false');
    row.innerHTML =
      '<svg class="arc-ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
      '<rect x="2" y="4" width="20" height="5" rx="1"/><path d="M4 9v10a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1V9"/><line x1="10" y1="13" x2="14" y2="13"/></svg>' +
      '<span class="arc-label">Archived</span>' +
      '<span class="arc-count">' + archivedPeers.length + '</span>' +
      (arcUnread > 0 ? '<span class="unread-badge muted">' + (arcUnread > 99 ? '99+' : arcUnread) + '</span>' : '');
    row.onclick = () => toggleArchivedView();
    row.onkeydown = (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleArchivedView(); } };
    list.appendChild(row);
  }

  if (!peers.length) {
    list.insertAdjacentHTML('beforeend', '<div class="contacts-empty">' +
      '<svg class="ce-ico" viewBox="0 0 24 24"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>' +
      (archivedPeers.length
        ? 'Everything is archived.<br><span>Open the archive above to bring a chat back.</span>'
        : 'No conversations yet.<br><span>Enter a username above to start chatting.</span>') +
      '</div>');
    return;
  }
  for (const peer of peers) {
    const msgs = chats[peer];
    const lastMsg = msgs.length ? msgs[msgs.length - 1] : null;
    const last = lastMsg ? lastMsg.body : '';
    // An icon and a short label instead of a long URL for attachments: the
    // list reads better and does not expose raw file links.
    const mt = lastMsg && lastMsg.media_type;
    const attIco = '<svg class="att-ico" viewBox="0 0 24 24"><path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>';
    const callIco = '<svg class="att-ico" viewBox="0 0 24 24"><path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07A19.5 19.5 0 0 1 4.69 12 19.79 19.79 0 0 1 1.61 3.4 2 2 0 0 1 3.6 1.22h3a2 2 0 0 1 2 1.72 12.84 12.84 0 0 0 .7 2.81 2 2 0 0 1-.45 2.11L7.91 8.84a16 16 0 0 0 6 6l.94-.94a2 2 0 0 1 2.11-.45 12.84 12.84 0 0 0 2.81.7A2 2 0 0 1 22 16.92z"/></svg>';
    const micIco = '<svg class="att-ico" viewBox="0 0 24 24"><path d="M12 1a3 3 0 0 0-3 3v8a3 3 0 0 0 6 0V4a3 3 0 0 0-3-3z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" y1="19" x2="12" y2="23"/></svg>';
    const prevHTML = mt
      ? (mt === 'call' ? callIco + (last.split(':')[0] === 'missed' ? 'Missed call' : 'Call')
         : mt === 'voice' ? micIco + 'Voice message'
         : attIco + (mt === 'image' ? 'Photo' : mt === 'video' ? 'Video' : escHtml(fileName(last) || 'File')))
      : escHtml(last);
    const div = document.createElement('div');
    const unread = countUnread(peer);
    const muted = (typeof chatIsMuted === 'function') && chatIsMuted(peer);
    div.className = 'contact-item' + (peer === activePeer ? ' active' : '') + (unread > 0 ? ' has-unread' : '') +
      (pins.has(peer) ? ' pinned' : '') + (muted ? ' muted' : '') + (archived(peer) ? ' archived' : '');
    div.id = 'contact-' + peer;
    // The peer lives in a data attribute: both right click and long press
    // find the row by delegation rather than a closure over a node, since
    // rows are re-created on every render.
    div.dataset.peer = peer;
    div.setAttribute('data-act-ctx', 'showChatMenu');
    div.onclick = () => openChat(peer);
    const isGrp = peer.startsWith('g:');
    // Group: a square avatar (no decorative glyph) plus "N online".
    // Direct chat: a round avatar plus a presence dot.
    const img = (!isGrp && typeof avatarImg === 'function') ? avatarImg(peer) : '';
    const avatarHTML = isGrp
      ? `<div class="contact-avatar is-group ${avatarClass(peer)}">${escHtml(displayName(peer)[0].toUpperCase())}</div>`
      : `<div class="contact-avatar ${avatarClass(peer)}${img ? ' has-img' : ''}">${img || escHtml(displayName(peer)[0].toUpperCase())}</div>`;
    let rightHTML;
    if (isGrp) {
      const on = groupOnlineCount(peer);
      rightHTML = `<div class="group-online${on ? '' : ' none'}" id="gon-${escHtml(peer)}"><span class="go-dot"></span>${on}</div>`;
    } else {
      const dotCls = (typeof presenceDotClass === 'function') ? presenceDotClass(peer) : ('contact-online-dot' + (onlineStatuses[peer] ? ' online' : ''));
      rightHTML = `<div class="${dotCls}" id="dot-${escHtml(peer)}"></div>`;
    }
    div.innerHTML = `
      ${avatarHTML}
      <div class="contact-info">
        <div class="contact-name">${pins.has(peer) ? '<span class="name-pin">' + PIN_SVG + '</span>' : ''}${escHtml(displayName(peer))}${muted ? '<span class="name-mute" title="Muted">' + MUTE_SVG + '</span>' : ''}</div>
        <div class="contact-preview">${prevHTML}</div>
      </div>
      ${unread > 0 ? '<div class="unread-badge' + (muted ? ' muted' : '') + '">' + (unread > 99 ? '99+' : unread) + '</div>' : ''}
      <button class="contact-pin" title="${pins.has(peer) ? 'Unpin' : 'Pin'}" aria-label="Pin conversation">${PIN_SVG}</button>
      ${rightHTML}`;
    const pinBtn = div.querySelector('.contact-pin');
    if (pinBtn) pinBtn.onclick = async (e) => {
      e.stopPropagation();
      const pinned = pinnedSet().has(peer);
      // Confirm, so a stray click does not toggle pinning back and forth.
      if (typeof hexConfirm !== 'function' || await hexConfirm((pinned ? 'Unpin' : 'Pin') + ' this conversation?')) {
        togglePin(peer);
      }
    };
    list.appendChild(div);
  }
}

function updateContact(peer, lastMsg) {
  renderContacts();
  updatePageTitle();
}

// How many group members other than me are online right now.
function groupOnlineCount(peer) {
  const g = (typeof groups !== 'undefined') ? groups[peer] : null;
  if (!g || !g.members) return 0;
  let n = 0;
  for (const u of Object.keys(g.members)) {
    // Count myself as online — I am looking at the list — and everyone
    // else by their current presence.
    if (u === myUsername || onlineStatuses[u]) n++;
  }
  return n;
}

// ── Typing indicators (1:1 and groups, multiple people) ──────────────────────
// _typers[peer] = { username: timeoutId }. In a group everyone typing is shown.
let _typers = {};
function addTyper(peer, user) {
  if (user === myUsername) return;
  if (!_typers[peer]) _typers[peer] = {};
  clearTimeout(_typers[peer][user]);
  _typers[peer][user] = setTimeout(() => { removeTyper(peer, user); }, 4500);
  renderTyping(peer);
}
function removeTyper(peer, user) {
  if (_typers[peer] && _typers[peer][user]) {
    clearTimeout(_typers[peer][user]);
    delete _typers[peer][user];
    renderTyping(peer);
  }
}
function renderTyping(peer) {
  const el = document.getElementById('typing-indicator');
  const nameEl = document.getElementById('typing-name');
  if (!el || !nameEl) return;
  const users = (activePeer === peer && _typers[peer]) ? Object.keys(_typers[peer]) : [];
  if (!users.length) { el.classList.remove('visible'); nameEl.textContent = ''; return; }
  let text;
  if (!peer.startsWith('g:')) {
    text = 'typing';
  } else if (users.length === 1) {
    text = users[0] + ' is typing';
  } else if (users.length === 2) {
    text = users[0] + ' and ' + users[1] + ' are typing';
  } else {
    text = users[0] + ', ' + users[1] + ' and ' + (users.length - 2) + ' more are typing';
  }
  nameEl.textContent = text;
  el.classList.add('visible');
}

function fmtDayLabel(ts) {
  const d   = new Date(ts);
  const now = new Date();
  const today     = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const yesterday = new Date(today - 86400000);
  const msgDay    = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  if (msgDay.getTime() === today.getTime())     return 'Today';
  if (msgDay.getTime() === yesterday.getTime()) return 'Yesterday';
  return d.toLocaleDateString('en-GB', { day: 'numeric', month: 'long', year: msgDay.getFullYear() !== now.getFullYear() ? 'numeric' : undefined });
}

// Remember the id of the last rendered message per peer, so only a genuinely
// new message is animated rather than the whole feed on every re-render:
// renderMessages rebuilds innerHTML wholesale, and a blanket animation would
// flicker on status changes, reactions and chat switches.
let _renderLastId = {};

// Download glyph for the button overlaid on images and video.
const dlIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>';

// Dynamic feed padding under the translucent bars. The feed scrolls beneath
// the header and the composer, so its padding matches those blocks' real
// heights and the first and last message are never hidden behind them. It is
// recomputed on any height change: a growing textarea, reply/edit panels, the
// search row, safe-area insets.
let _feedInsetRO = null;
function initFeedInsets() {
  if (_feedInsetRO) return;
  const top = document.getElementById('chat-top');
  const bottom = document.getElementById('chat-bottom');
  const wrap = document.getElementById('messages-wrap');
  if (!top || !bottom || !wrap || typeof ResizeObserver === 'undefined') return;
  // The variables go on the parent, so both the feed (padding) and the
  // floating date badge (its top) inherit them and the date clears the header.
  const host = wrap.parentElement || wrap;
  const apply = () => {
    // Were we at the bottom before the padding change? Keeps the bottom pinned as the composer grows.
    const atBottom = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 60;
    host.style.setProperty('--feed-pad-top', top.offsetHeight + 'px');
    host.style.setProperty('--feed-pad-bottom', bottom.offsetHeight + 'px');
    if (atBottom) wrap.scrollTop = wrap.scrollHeight;
  };
  _feedInsetRO = new ResizeObserver(apply);
  _feedInsetRO.observe(top);
  _feedInsetRO.observe(bottom);
  apply();
  window.addEventListener('resize', apply);
}

function renderMessages(peer) {
  const wrap = document.getElementById('messages-wrap');
  // Was the user at the bottom? Capture it NOW, before innerHTML='' resets
  // scrollTop to 0 — otherwise the post-rebuild check always reads scrollTop=0,
  // decides "not at bottom", and the chat flies to the top on send/receive.
  const wasAtBottom = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 150;
  sortChat(peer);
  const msgs = chats[peer] || [];
  const _curLastId = msgs.length ? msgs[msgs.length - 1].id : null;
  const _animateLast = _renderLastId[peer] && _curLastId && _renderLastId[peer] !== _curLastId;
  _renderLastId[peer] = _curLastId;
  wrap.innerHTML = '';
  let lastDayKey = null;
  let _lastRow = null;
  for (const m of msgs) {
    const isOut = m.from === myUsername;
    const row = document.createElement('div');
    row.className = 'msg-row ' + (isOut ? 'out' : 'in');
    const tsVal = m.created_at || m.ts || (m.id ? parseInt(m.id.split('-')[0]) : Date.now());
    const msgTime = new Date(tsVal);

    // ── Day separator ──────────────────────────────────────────────────────
    const dayKey = msgTime.getFullYear() + '-' + msgTime.getMonth() + '-' + msgTime.getDate();
    if (dayKey !== lastDayKey) {
      lastDayKey = dayKey;
      const dayDiv = document.createElement('div');
      dayDiv.className = 'msg-day';
      dayDiv.textContent = fmtDayLabel(tsVal);
      wrap.appendChild(dayDiv);
    }

    const time = msgTime.getHours().toString().padStart(2,'0') + ':' + msgTime.getMinutes().toString().padStart(2,'0');
    const statusIcon = (m.status === 'delivered' || m.status === 'read') ? '✓✓' : (m.status === 'failed' ? '⚠' : m.status === 'sent' ? '✓' : '···');
    const statusHTML = isOut && m.id
      ? `<span class="msg-status ${m.status || 'sending'}" id="msg-status-${escHtml(m.id)}">${statusIcon}</span>`
      : '';
    const menuBtn = m.id && !m.deleted
      ? `<button class="msg-menu-btn" data-act="showCtxMenu" data-a1="${escHtml(m.id)}" data-a2="${isOut}" data-stop title="More" aria-label="Message actions">⋯</button>`
      : '';
    const editedMark = m.edited && !m.deleted
      ? `<span class="msg-edited">· edited</span>`
      : '';

    // Forward badge
    const fwdBadge = m.forwarded ? `<span class="fwd-badge">↪ Forwarded</span>` : '';

    // Reply quote
    let replyHTML = '';
    if (m.reply_to) {
      const orig = msgs.find(x => x.id === m.reply_to);
      const origText = orig ? (orig.body.startsWith('/files/') ? 'File' : orig.body.substring(0,60)) : '...';
      const origFrom = orig ? orig.from : '';
      replyHTML = `<div class="msg-reply-quote" data-act="scrollToMsg" data-a1="${escHtml(m.reply_to)}"><strong>${escHtml(origFrom)}</strong>${escHtml(origText)}</div>`;
    }

    let bodyHTML;
    if (m.deleted) {
      bodyHTML = `🫠<span class="msg-deleted"> Message deleted</span>`;
    } else if (m.media_type === 'call') {
      bodyHTML = (typeof callLogHtml === 'function') ? callLogHtml(m, isOut) : '📞 Call';
    } else if (m.media_type === 'voice') {
      bodyHTML = voiceBubbleHtml(m);
    } else if (m.media_type === 'image') {
      const u = escHtml(m.body).replace(/'/g, '%27');
      bodyHTML = `<div class="media-wrap"><img class="msg-image"${mediaSizeAttr(m.body)} src="${escHtml(m.body)}" data-act="openMedia" data-a1="${u}"/>` +
        `<button class="media-dl-btn" title="Download" aria-label="Download" data-act="downloadFile" data-a1="${u}" data-stop>${dlIcon}</button></div>`;
    } else if (m.media_type === 'video') {
      const u = escHtml(m.body).replace(/'/g, '%27');
      bodyHTML = `<div class="media-wrap"><video class="msg-video"${mediaSizeAttr(m.body)} src="${escHtml(m.body)}" controls preload="metadata"></video>` +
        `<button class="media-dl-btn" title="Download" aria-label="Download" data-act="downloadFile" data-a1="${u}" data-stop>${dlIcon}</button></div>`;
    } else if (m.media_type === 'document') {
      const fname = fileName(m.body);
      const u = escHtml(m.body).replace(/'/g, '%27');
      bodyHTML = `<a class="msg-file" href="${escHtml(fileUrl(m.body))}" download data-act="downloadFile" data-a1="${u}" data-prevent>
        <svg viewBox="0 0 24 24"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/></svg>
        ${escHtml(fname)}</a>`;
    } else {
      const MAX = 2000;
      if (m.body && m.body.length > MAX && !m._expanded) {
        bodyHTML = `<span class="msg-longtext">${escHtml(m.body.slice(0, MAX))}…</span>` +
          `<button class="msg-expand" data-act="expandMsg" data-a1="${escHtml(m.id)}" data-a2="${escHtml(peer)}">Show full message (${m.body.length.toLocaleString()})</button>`;
      } else {
        bodyHTML = (typeof linkifyText === 'function') ? linkifyText(m.body || '') : escHtml(m.body);
      }
      // Lazy Open Graph preview card under the bubble (filled by linkpreview.js).
      const _lpUrl = (typeof firstUrl === 'function') ? firstUrl(m.body) : null;
      if (_lpUrl && !m._expanded) bodyHTML += `<div class="lp-slot" data-url="${escHtml(_lpUrl)}"></div>`;
    }

    const reactionsHTML = reactionsBarHtml(m);

    const senderHTML = (peer.startsWith('g:') && !isOut)
      ? `<div class="msg-sender ${avatarClass(m.from)}-t">${escHtml(m.from)}</div>` : '';
    // Bubble and reactions live in a wrapper column, with reactions on their
    // own row below the bubble: the bubble's size does not change when a
    // reaction appears, so nothing jumps and nothing is clipped.
    row.innerHTML = `<div class="msg-col"><div class="msg-bubble" data-id="${escHtml(m.id)}" data-act-ctx="showCtxMenu" data-a1="${escHtml(m.id)}" data-a2="${isOut}">${senderHTML}${menuBtn}${fwdBadge}${replyHTML}${bodyHTML}<span class="msg-time">${time}${editedMark}${statusHTML}</span></div>${reactionsHTML}</div>`;
    wrap.appendChild(row);
    _lastRow = row;
  }
  if (_animateLast && _lastRow) _lastRow.classList.add('msg-in');
  // Scroll down only if the user was already at the bottom (captured before clearing).
  if (wasAtBottom) wrap.scrollTop = wrap.scrollHeight;
  // Remember the intent to stay at the bottom, so loading media does not shift the chat.
  wrap._stickBottom = wasAtBottom;
  // Direct load handlers on each media element (delegation does not work in Safari).
  bindMediaLoadHandlers(wrap);
  // Fetch link-preview cards lazily from /unfurl.
  if (typeof hydrateLinkPreviews === 'function') hydrateLinkPreviews(wrap);
}

// The reactions bar for one message. An empty string when there are none,
// including the case where every user is left with an empty array after
// removing theirs, which used to render an empty bar.
function reactionsBarHtml(m) {
  if (!m.reactions || !Object.keys(m.reactions).length) return '';
  const counts = {};
  const mine = new Set();
  for (const [uid, emojis] of Object.entries(m.reactions)) {
    for (const e of emojis) {
      counts[e] = (counts[e] || 0) + 1;
      if (uid === myUsername) mine.add(e);
    }
  }
  const chips = Object.entries(counts).map(([e, n]) =>
    `<span class="reaction-chip${mine.has(e) ? ' mine' : ''}" data-act="toggleReaction" data-a1="${escHtml(m.id)}" data-a2="${e}">${e}<span class="rcount">${n}</span></span>`
  ).join('');
  return chips ? '<div class="reactions-bar">' + chips + '</div>' : '';
}

// Update one message's reactions in place: a reaction is the most frequent
// small event, and a full renderMessages rebuilds the entire feed's innerHTML
// (videos blink, selection is lost). The DOM structure mirrors
// renderMessages: .msg-col > [.msg-bubble[data-id], .reactions-bar?].
function updateReactionsBar(peer, msgId) {
  if (peer !== activePeer || !msgId) return;
  const esc = (window.CSS && CSS.escape) ? CSS.escape(msgId) : msgId;
  const bubble = document.querySelector(`.msg-bubble[data-id="${esc}"]`);
  if (!bubble) { renderMessages(peer); return; } // not rendered yet — fall back
  const m = (chats[peer] || []).find(x => x.id === msgId);
  if (!m) return;
  const wrap = document.getElementById('messages-wrap');
  const atBottom = wrap && wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 150;
  const col = bubble.parentElement;
  const existing = col && col.querySelector('.reactions-bar');
  if (existing) existing.remove();
  const html = reactionsBarHtml(m);
  if (html) bubble.insertAdjacentHTML('afterend', html);
  // The bar under the last message changes the feed's height, so the bottom
  // is held exactly as a full renderMessages would.
  if (atBottom && wrap) wrap.scrollTop = wrap.scrollHeight;
}

function scrollToMsg(id) {
  const el = document.querySelector(`[data-id="${id}"]`);
  if (!el) return;
  el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  el.style.transition = 'background .3s';
  el.style.background = 'rgba(0,49,83,.5)';
  setTimeout(() => {
    el.style.transition = 'background 1s';
    el.style.background = '';
  }, 1500);
}

// ── Send ──────────────────────────────────────────────────

// Edit an existing (own, text) message: POST to the server, then update locally.
function submitEdit(peer, msgID, text) {
  // Applied optimistically: the change is local first and the server confirms
  // through the "edited" WS event. The fetch is fire-and-forget because on
  // mobile it often aborts when the keyboard closes, after the server has
  // already processed the request.
  applyEdit(peer, msgID, text);
  cancelEdit();
  fetch(`${location.protocol}//${SERVER}/edit-message`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` },
    body: JSON.stringify({ msg_id: msgID, body: text }),
  }).catch(() => {});
}

// Update a message body locally (in-memory + storage) and re-render.
function applyEdit(peer, msgID, newBody) {
  const m = (chats[peer] || []).find(x => x.id === msgID);
  if (m) { m.body = newBody; m.edited = true; }
  try {
    const k = `hc_msgs_${myUsername}_${peer}`;
    const stored = JSON.parse(localStorage.getItem(k) || '[]');
    const sm = stored.find(x => x.id === msgID);
    if (sm) { sm.body = newBody; sm.edited = true; localStorage.setItem(k, JSON.stringify(stored)); }
  } catch {}
  if (activePeer === peer) renderMessages(peer);
}
