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
  // A new direct conversation is created only with an existing user: a typo in a
  // username used to spawn an eternal "ghost conversation" in the sidebar.
  if (!chats[peer] && !peer.startsWith('g:')) {
    fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(peer)}`, {
      headers: { 'Authorization': `Bearer ${token}` }
    }).then(r => {
      // The username was typed by hand — we always ask, even if it is marked
      // deleted: this is a rare explicit action, and its answer updates the
      // memory in both directions.
      if (r.status === 404) {
        if (typeof markPeerGone === 'function') markPeerGone(peer);
        toast('User "' + peer + '" does not exist.'); return;
      }
      if (!r.ok && r.status !== 404) return; // network/server — do not block silently
      if (typeof unmarkPeerGone === 'function') unmarkPeerGone(peer);
      chats[peer] = [];
      _openChatInner(peer);
    }).catch(() => { chats[peer] = []; _openChatInner(peer); });
    return;
  }
  if (!chats[peer]) chats[peer] = [];
  _openChatInner(peer);
}

function _openChatInner(peer) {

  // Save the scroll position of the previous chat
  if (activePeer) {
    const wrap = document.getElementById('messages-wrap');
    if (wrap) scrollPositions[activePeer] = wrap.scrollTop;
  }

  activePeer = peer;
  saveActivePeer(peer);
  if (typeof closeChatSearch === 'function') closeChatSearch();
  // Reply is scoped to a chat: on leaving a conversation we reset an unfinished
  // reply, otherwise the "reply" preview from chat A leaked into chat B (and
  // referred to someone else's message — "..."). Editing is reset only when it is
  // active — so an ordinary chat switch does not wipe the draft in the input field.
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
  // The attachments panel is open — it must follow the selected conversation.
  // Otherwise the header shows one name with another person's files under it:
  // that is not "stale data" but plain misinformation.
  if (typeof attachPanelFollowPeer === 'function') attachPanelFollowPeer(peer);

  // Restore the scroll position or scroll to the end
  const wrap = document.getElementById('messages-wrap');
  if (wrap) {
    if (scrollPositions[peer] !== undefined) {
      wrap.scrollTop = scrollPositions[peer];
      // Hold the bottom only if the saved position was at the bottom too.
      wrap._stickBottom = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 150;
    } else {
      wrap.scrollTop = wrap.scrollHeight;
      // We enter at the end of the conversation — and stay stuck to the bottom
      // while photos/videos load (otherwise the feed jumps up in Safari).
      wrap._stickBottom = true;
    }
  }

  // Mark the whole conversation read on opening
  if (ws && ws.readyState === 1) {
    if (!peer.startsWith('g:')) ws.send(JSON.stringify({ type: 'read', from: myUsername, to: peer }));
  }

  // The peer's profile (name/title/avatar/presence): the contact could have just
  // been opened and not yet made it into the preloaded peerList.
  if (!peer.startsWith('g:') && typeof fetchPeerProfile === 'function') fetchPeerProfile(peer);

  // We request the peer's current online status when the chat opens.
  // For a group we do NOT ask: the asynchronous answer overwrote the
  // "N members" header with "offline" (to /status a group is a non-existent user).
  // A deleted account is not queried: the answer is known in advance — 404.
  if (!peer.startsWith('g:') && !(typeof peerIsGone === 'function' && peerIsGone(peer))) {
    fetch(`${location.protocol}//${SERVER}/status?user=${encodeURIComponent(peer)}`, {
      headers: { 'Authorization': `Bearer ${token}` }
    }).then(r => {
      if (r.status === 404 && typeof markPeerGone === 'function') markPeerGone(peer);
      return r.ok ? r.json() : null;
    }).then(d => {
      if (d) {
        onlineStatuses[peer] = d.online;
        const dot = document.getElementById('dot-' + peer);
        if (dot) dot.className = (typeof presenceDotClass === 'function') ? presenceDotClass(peer) : ('contact-online-dot' + (d.online ? ' online' : ''));
        if (activePeer === peer) showChatHeader(peer);
      }
    }).catch(() => {});
  }

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
    // ONLY "Manage" (with the icon on the left) is clickable, not the whole header.
    statusEl.innerHTML = n + (n === 1 ? ' member' : ' members') +
      (on ? ' · <span style="color:var(--online)">' + on + ' online</span>' : '') +
      ' · <span class="gp-manage" data-act="openGroupPanel" data-stop title="Group members &amp; settings">' +
      '<svg viewBox="0 0 24 24"><path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87"/></svg>Manage</span>';
    statusEl.style.color = 'var(--muted)';
    for (const el of [statusEl, nameEl, av]) {
      el.style.cursor = ''; el.onclick = null; el.removeAttribute('title');
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
      // removeAttribute rather than title = '': an empty title still remains an
      // attribute in the markup, leaving nothing to check "there is no label" with.
      el.removeAttribute('title');
    }
  }
  // Calls in groups are v2 (mesh/SFU); the buttons are hidden so we do not run
  // into the broken "offer to a group" scenario.
  document.querySelectorAll('.call-start-btn').forEach(b => b.style.display = isGrp ? 'none' : '');
  // The top-right group icon is no longer needed: management opens from "Manage"
  // next to the group membership.
  // The group management button in the header. On mobile the status row (with
  // "Manage") is hidden to save space, so for groups we show this icon — it is
  // always available in portrait orientation.
  const gib = document.getElementById('group-info-btn');
  if (gib) gib.style.display = isGrp ? 'flex' : 'none';
}


// ── Unread counters ──────────────────────────────────────
// lastReadSeq[peer] = the seq of the last message the user "saw" (opened the
// chat). Everything with seq > lastReadSeq and from !== myUsername is unread.
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

// ── Pin: keeping chats/groups at the top. A personal device setting —
// stored locally (needs no server or schema). ──
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
// A struck-through bell means a muted conversation. The same icon as on the menu
// item, so the link "pressed Mute → got this" reads immediately.
const MUTE_SVG = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M13.73 21a2 2 0 0 1-3.46 0"/><path d="M18.63 13A17.9 17.9 0 0 1 18 8"/><path d="M6.26 6.26A5.86 5.86 0 0 0 6 8c0 7-3 9-3 9h14"/><path d="M18 8a6 6 0 0 0-9.33-5"/><line x1="1" y1="1" x2="23" y2="23"/></svg>';

// closeActiveChat — leave a conversation without opening a neighbouring one.
// Needed when the open chat disappears from the list (archived, deleted): without
// it, it stayed on screen even though its sidebar row was already gone.
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

// Is the "Archive" section expanded? Through typeof rather than directly:
// chatmenu.js is loaded after this file, and before it runs the variable does not
// exist yet — the list must render rather than fall over.
function _archiveOpen() {
  return (typeof showArchivedList === 'function') && showArchivedList();
}

function renderContacts() {
  const list = document.getElementById('contacts-list');
  // We only show the groups we are a member of: the messages of a group that was
  // left stay in localStorage, and without this filter it hung in the sidebar as
  // a ghost "Group" with an empty membership and no way out.
  const pins = pinnedSet();
  const archived = (p) => (typeof chatIsArchived === 'function') && chatIsArchived(p);
  const visible = Object.keys(chats).filter(p =>
    !p.startsWith('g:') || (typeof groups !== 'undefined' && groups[p])
  );
  // The archive is a separate section, not a "show everything" filter: mixing
  // put-away conversations with active ones means bringing back the very list the
  // archive was needed for.
  const archivedPeers = visible.filter(archived);

  // Conversation freshness — for the active list. lastActivity is computed once
  // per conversation rather than inside the comparator: there it was recomputed
  // on every comparison, that is, O(n log n) times instead of n.
  const lastActivity = (p) => {
    const m = chats[p];
    if (!m || !m.length) return 0;
    const last = m[m.length - 1];
    return last.ts || parseInt(last.id) || 0;
  };
  const archivedAt = (p) => (typeof chatArchivedAt === 'function') ? chatArchivedAt(p) : 0;

  const peers = visible.filter(p => !archived(p) || _archiveOpen()).sort((a, b) => {
    // The archive goes RIGHT UNDER its own row, that is, at the head of the list.
    //
    // It used to go to the very bottom, and the two ended up apart: the "Archived"
    // heading is drawn first (before the loop, see below) while the section's
    // contents ended up after all the active chats. A person expanded the section
    // and saw no result — they had to scroll to the end of the list to find what
    // they had expanded. A heading and its contents must stand together.
    const aa = archived(a), ab = archived(b);
    if (aa !== ab) return aa ? -1 : 1;
    if (aa) {
      // Inside the archive — by the time of archiving, the most recently put
      // away on top. Sorting archived conversations by message freshness makes
      // no sense: these are precisely the chats no messages are expected from,
      // and someone else's remark would reshuffle the whole section. Rows without
      // a stamp (archived before archived_at existed) go to the bottom rather
      // than the top: 0 would otherwise mean "just put away".
      const ta = archivedAt(a) || 0, tb = archivedAt(b) || 0;
      if (ta !== tb) return tb - ta;
      return a < b ? -1 : 1;            // same time — by name, stably
    }
    const pa = pins.has(a), pb = pins.has(b);
    if (pa !== pb) return pa ? -1 : 1;   // pinned ones — always on top
    return lastActivity(b) - lastActivity(a);
  });
  list.innerHTML = '';

  // The "Archived (N)" row is the entry to the section. Shown only when there is
  // something in the archive: an empty menu item is noise.
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
    // While the history is still loading, "No conversations yet" is not an
    // absence of information but an INCORRECT statement: the conversations exist,
    // they have simply not been fetched yet. A person reads it as "everything is
    // lost". So until loading finishes there is a skeleton in this place: it
    // honestly says "coming up", takes the same height as the future rows, and
    // so the list does not jump when the real data is substituted.
    const stillLoading = (typeof _historyLoading !== 'undefined' && _historyLoading) &&
                         !archivedPeers.length;
    if (stillLoading) {
      list.insertAdjacentHTML('beforeend',
        '<div class="contacts-skeleton" aria-hidden="true">' +
        '<div class="sk-row"><div class="sk-av"></div>' +
        '<div class="sk-lines"><div class="sk-line sk-w60"></div><div class="sk-line sk-w85"></div></div></div>'.repeat(4) +
        '</div>' +
        // A screen reader does not need blinking rectangles — it needs a word.
        '<div class="sr-only" role="status" aria-live="polite">Loading conversations</div>');
      return;
    }
    list.insertAdjacentHTML('beforeend', '<div class="contacts-empty">' +
      '<svg class="ce-ico" viewBox="0 0 24 24"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>' +
      (archivedPeers.length
        ? 'Everything is archived.<br><span>Open the archive above to bring a chat back.</span>'
        : 'No conversations yet.<br><span>Enter a username above to start chatting.</span>') +
      '</div>');
    return;
  }
  // Section headings. They appear ONLY when there is something to separate:
  // without pinned conversations a lone "All" caption over a flat list is pure
  // noise. Plus the archive section, which is drawn as its own row above.
  const hasPins = peers.some(p => pins.has(p) && !archived(p));
  let sectionDrawn = { pinned: false, all: false };
  const drawSection = (label) => {
    const h = document.createElement('div');
    h.className = 'list-section';
    h.textContent = label;
    list.appendChild(h);
  };

  for (const peer of peers) {
    if (hasPins && !archived(peer)) {
      if (pins.has(peer) && !sectionDrawn.pinned) { sectionDrawn.pinned = true; drawSection('Pinned'); }
      if (!pins.has(peer) && !sectionDrawn.all)   { sectionDrawn.all = true;   drawSection('All chats'); }
    }
    const msgs = chats[peer];
    const lastMsg = msgs.length ? msgs[msgs.length - 1] : null;
    const last = lastMsg ? lastMsg.body : '';
    // An icon plus a short label instead of a long URL for media attachments —
    // the list looks livelier and does not show raw file links.
    const mt = lastMsg && lastMsg.media_type;
    const attIco = '<svg class="att-ico" viewBox="0 0 24 24"><path d="M21.44 11.05l-9.19 9.19a6 6 0 0 1-8.49-8.49l9.19-9.19a4 4 0 0 1 5.66 5.66l-9.2 9.19a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>';
    const callIco = '<svg class="att-ico" viewBox="0 0 24 24"><path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07A19.5 19.5 0 0 1 4.69 12 19.79 19.79 0 0 1 1.61 3.4 2 2 0 0 1 3.6 1.22h3a2 2 0 0 1 2 1.72 12.84 12.84 0 0 0 .7 2.81 2 2 0 0 1-.45 2.11L7.91 8.84a16 16 0 0 0 6 6l.94-.94a2 2 0 0 1 2.11-.45 12.84 12.84 0 0 0 2.81.7A2 2 0 0 1 22 16.92z"/></svg>';
    const micIco = '<svg class="att-ico" viewBox="0 0 24 24"><path d="M12 1a3 3 0 0 0-3 3v8a3 3 0 0 0 6 0V4a3 3 0 0 0-3-3z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" y1="19" x2="12" y2="23"/></svg>';
    const prevHTML = mt
      ? (mt === 'call' ? callIco + (last.split(':')[0] === 'missed' ? 'Missed call' : 'Call')
         : mt === 'voice' ? micIco + 'Voice message'
         // The real file name rather than "Photo"/"Video". A person scans for
         // "design-review.png", and a generic word does not help with that.
         // The fallback remains for old messages without a name.
         : attIco + escHtml(fileName(last) ||
             (mt === 'image' ? 'Photo' : mt === 'video' ? 'Video' : 'File')))
      : escHtml(typeof rtStripMarkup === 'function' ? rtStripMarkup(last) : last);
    const div = document.createElement('div');
    const unread = countUnread(peer);
    const muted = (typeof chatIsMuted === 'function') && chatIsMuted(peer);
    div.className = 'contact-item' + (peer === activePeer ? ' active' : '') + (unread > 0 ? ' has-unread' : '') +
      (pins.has(peer) ? ' pinned' : '') + (muted ? ' muted' : '') + (archived(peer) ? ' archived' : '');
    div.id = 'contact-' + peer;
    // peer in a data attribute: both right-click and long press find the row
    // through delegation rather than through a closure on a specific node — rows
    // are recreated on every render.
    div.dataset.peer = peer;
    div.setAttribute('data-act-ctx', 'showChatMenu');
    div.onclick = () => openChat(peer);
    const isGrp = peer.startsWith('g:');
    // A group: a square avatar (without a decorative badge) + "N online".
    // A direct chat: a round avatar + a presence dot.
    const img = (!isGrp && typeof avatarImg === 'function') ? avatarImg(peer) : '';
    const avatarInner = isGrp
      ? `<div class="contact-avatar is-group ${avatarClass(peer)}">${escHtml(displayName(peer)[0].toUpperCase())}</div>`
      : `<div class="contact-avatar ${avatarClass(peer)}${img ? ' has-img' : ''}">${img || escHtml(displayName(peer)[0].toUpperCase())}</div>`;
    // The presence indicator moved ONTO the avatar. It used to occupy a separate
    // place on the right — that is, the right edge of the row was given over to a
    // lone 8-pixel dot, leaving no room for the time and the counter. On the
    // avatar the dot reads just as well (it is next to the face it refers to),
    // and the freed column fills the row with something useful.
    //
    // The element id is unchanged (dot-<peer>): the live presence update from
    // transport.js finds the dot by it.
    let badgeHTML;
    if (isGrp) {
      const on = groupOnlineCount(peer);
      badgeHTML = `<div class="group-online${on ? '' : ' none'}" id="gon-${escHtml(peer)}"><span class="go-dot"></span>${on}</div>`;
    } else {
      const dotCls = (typeof presenceDotClass === 'function') ? presenceDotClass(peer) : ('contact-online-dot' + (onlineStatuses[peer] ? ' online' : ''));
      // Deliberately without a positioning class: the live presence update
      // rewrites className wholesale, and such a class would not survive the
      // first status change. The position is set by the `.contact-av > …` selector.
      badgeHTML = `<div class="${dotCls}" id="dot-${escHtml(peer)}"></div>`;
    }
    const avatarHTML = `<div class="contact-av">${avatarInner}${badgeHTML}</div>`;
    const ts = lastMsg ? (lastMsg.ts || parseInt(lastMsg.id) || 0) : 0;
    const timeTxt = (typeof contactTime === 'function') ? contactTime(ts) : '';
    div.innerHTML = `
      ${avatarHTML}
      <div class="contact-info">
        <div class="contact-name">${pins.has(peer) ? '<span class="name-pin">' + PIN_SVG + '</span>' : ''}${escHtml(displayName(peer))}${muted ? '<span class="name-mute" title="Muted">' + MUTE_SVG + '</span>' : ''}</div>
        <div class="contact-preview">${prevHTML}</div>
      </div>
      <div class="contact-meta">
        <div class="contact-time">${escHtml(timeTxt)}</div>
        <button class="contact-pin" title="${pins.has(peer) ? 'Unpin' : 'Pin'}" aria-label="Pin conversation">${PIN_SVG}</button>
        ${unread > 0 ? '<div class="unread-badge' + (muted ? ' muted' : '') + '">' + (unread > 99 ? '99+' : unread) + '</div>' : ''}
      </div>`;
    const pinBtn = div.querySelector('.contact-pin');
    if (pinBtn) pinBtn.onclick = async (e) => {
      e.stopPropagation();
      const pinned = pinnedSet().has(peer);
      // A confirmation — so a stray click does not toggle pinning back and forth.
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

// How many group members (apart from me) are online right now.
function groupOnlineCount(peer) {
  const g = (typeof groups !== 'undefined') ? groups[peer] : null;
  if (!g || !g.members) return 0;
  let n = 0;
  for (const u of Object.keys(g.members)) {
    // We always count ourselves as online (we are connected, since we are looking
    // at the list); the others go by their current presence status.
    if (u === myUsername || onlineStatuses[u]) n++;
  }
  return n;
}

// ── Typing indicators (1:1 and groups, multiple people) ──────────────────────
// _typers[peer] = { username: timeoutId }. In a group we show everyone who is typing.
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

// We remember the id of the last rendered message per peer so that ONLY a
// genuinely new message is animated rather than the whole feed on every repaint
// (renderMessages rebuilds the entire innerHTML — a blanket animation would make
// statuses, reactions and chat switching flicker).
let _renderLastId = {};

// The "download" icon for the button over pictures/videos.
const dlIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/></svg>';

// Dynamic feed padding under the translucent bars. The feed (#messages-wrap)
// scrolls UNDER the header (#chat-top) and the input field (#chat-bottom), so it
// is given padding equal to the real height of those blocks — so the first and
// last message do not hide behind a bar. Recomputed on any height change: the
// textarea growing, reply/edit panels appearing, the search row, the safe area.
let _feedInsetRO = null;
function initFeedInsets() {
  if (_feedInsetRO) return;
  const top = document.getElementById('chat-top');
  const bottom = document.getElementById('chat-bottom');
  const wrap = document.getElementById('messages-wrap');
  if (!top || !bottom || !wrap || typeof ResizeObserver === 'undefined') return;
  // The variables are set on #chat-main (the parent): they are inherited both by
  // the feed (padding) and by the floating date badge (its top), so the date does
  // not slide under the header.
  const host = wrap.parentElement || wrap;
  const apply = () => {
    // Whether we were pinned to the bottom BEFORE the padding changed — so the
    // bottom is held while the input field grows. The flag from gestures.js is
    // updated on scroll, that is, IN ADVANCE: by the time we are called here the
    // sizes may already have changed (the keyboard shrinks the layout), and
    // measuring "are we at the bottom" right now is too late — see the comment
    // on feedStuckToBottom.
    const atBottom = (typeof window.feedStuckToBottom === 'boolean')
      ? window.feedStuckToBottom
      : wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 60;
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

// ── Message fingerprint ──────────────────────────────────────────────────
// A string that changes exactly when a message row needs to be redrawn: text,
// status, edit, deletion, reactions, expanding a long body. Needed for the
// incremental rendering below — comparing an array of strings is incomparably
// cheaper than rebuilding the DOM.
// _hash — a short digest of a string (djb2). It is needed so the fingerprint
// depends on the CONTENT rather than the length: editing "server is down" into
// "server is back" does not change the length, and if a message is edited a
// second time the edited flag is already set — the length-based fingerprint
// would match and the new text would simply never appear on screen.
function _hash(str) {
  let h = 5381;
  for (let i = 0; i < str.length; i++) h = ((h << 5) + h + str.charCodeAt(i)) | 0;
  return h;
}

function _msgFp(m) {
  const body = m.body || '';
  return [
    m.id, m.status || '', m.edited ? 1 : 0, m.deleted ? 1 : 0,
    m._expanded ? 1 : 0, body.length, _hash(body), m.rseq || 0,
    m.reactions ? JSON.stringify(m.reactions) : '',
  ].join('|');
}
let _pendingIds = null;
const _renderedFp = {};    // peer → fingerprints of the rows already drawn
const _renderedDay = {};   // peer → day key of the last row

// For how many minutes consecutive messages from one sender count as a single
// "utterance". Five is a compromise: two phrases in a row are joined, while a
// reply half an hour later is separated by time and reads as a new approach.
const GROUP_WINDOW_MS = 5 * 60 * 1000;

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

  // ── Incremental rendering ──────────────────────────────────────────────
  // Every incoming message used to wipe the whole feed (innerHTML = '') and
  // assemble it again. In a conversation of a hundred messages that is
  // imperceptible; at several thousand it is hundreds of created nodes per
  // EVERY message, and the feed lags noticeably at exactly the moment the
  // conversation is lively.
  //
  // Here we compare fingerprints: if the previous rows are unchanged and the list
  // has only grown at the end, we draw in the missing ones. Any other difference —
  // an edit, a deletion, a reaction, a status change, opening another chat —
  // honestly leads to a full rebuild, as before.
  const fps = msgs.map(_msgFp);
  const prevFps = _renderedFp[peer];
  let from = 0;                       // which message to start drawing from
  let appendOnly = false;
  if (wrap.dataset.peer === peer && prevFps && prevFps.length &&
      prevFps.length <= fps.length && wrap.children.length) {
    appendOnly = true;
    for (let i = 0; i < prevFps.length; i++) {
      if (prevFps[i] !== fps[i]) { appendOnly = false; break; }
    }
    if (appendOnly) from = prevFps.length;
  }
  _renderedFp[peer] = fps;
  wrap.dataset.peer = peer;

  // Nothing changed — no reason to touch the DOM at all.
  if (appendOnly && from === fps.length) {
    if (wasAtBottom) wrap.scrollTop = wrap.scrollHeight;
    return;
  }

  // The queue is read once per render rather than per message: isQueued goes
  // into localStorage and parses JSON, and in a loop over the feed that came to
  // hundreds of parses in a row.
  _pendingIds = (typeof pendingMessageIds === 'function') ? pendingMessageIds() : null;

  let lastDayKey = null;
  if (appendOnly) {
    lastDayKey = _renderedDay[peer] || null;
  } else {
    wrap.innerHTML = '';
  }
  // Texts to be parsed into nodes once the rows are in the DOM. A pair
  // [message id, source text] — the source, not markup: that is what is in the
  // database too.
  _pendingRich.length = 0;
  let _lastRow = null;
  for (let mi = from; mi < msgs.length; mi++) {
    const m = msgs[mi];
    const prevM = mi > 0 ? msgs[mi - 1] : null;
    const isOut = m.from === myUsername;
    const row = document.createElement('div');
    row.className = 'msg-row ' + (isOut ? 'out' : 'in');
    const tsVal = m.created_at || m.ts || (m.id ? parseInt(m.id.split('-')[0]) : Date.now());
    const msgTime = new Date(tsVal);

    // ── Day separator ──────────────────────────────────────────────────────
    const dayKey = msgTime.getFullYear() + '-' + msgTime.getMonth() + '-' + msgTime.getDate();
    const newDay = dayKey !== lastDayKey;
    if (newDay) {
      lastDayKey = dayKey;
      const dayDiv = document.createElement('div');
      dayDiv.className = 'msg-day';
      dayDiv.textContent = fmtDayLabel(tsVal);
      wrap.appendChild(dayDiv);
    }

    // ── Grouping consecutive messages ──────────────────────────────────────
    // Three messages in a row from one person are one thought, not three cards.
    // On a continuation we drop the repeated name in groups and tighten the top
    // margin; a day separator always breaks the group.
    if (!newDay && prevM && prevM.from === m.from && !prevM.deleted && !m.deleted) {
      const prevTs = prevM.created_at || prevM.ts ||
        (prevM.id ? parseInt(prevM.id.split('-')[0]) : 0);
      if (tsVal - prevTs < GROUP_WINDOW_MS) row.classList.add('grouped');
    }

    const time = msgTime.getHours().toString().padStart(2,'0') + ':' + msgTime.getMinutes().toString().padStart(2,'0');
    const statusIcon = (m.status === 'delivered' || m.status === 'read') ? '✓✓' : (m.status === 'failed' ? '⚠' : m.status === 'sent' ? '✓' : '···');
    // "Queued" is not the same as "sending". A message written without a
    // connection sits in the persistent outbox and goes out by itself on a
    // reconnect; it cannot be lost. But in the feed it had the same faint "···"
    // badge as an ordinary 50 ms send, and the person could not tell "about to
    // go" from "waiting for the network". We mark a class on the row — the style
    // below makes such a bubble muted.
    const queued = isOut && m.id && (!m.status || m.status === 'sending') &&
      _pendingIds && _pendingIds.has(m.id);
    if (queued) row.classList.add('queued');
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
      // src goes through mediaSrc: it also busts the cache in which some clients
      // have stale 404s stuck (see MEDIA_CACHE_BUST in helpers.js).
      bodyHTML = `<div class="media-wrap"><img class="msg-image"${mediaSizeAttr(m.body)} src="${escHtml(mediaSrc(m.body))}" data-act="openMedia" data-a1="${u}" data-a2="${escHtml(fileName(m.body))}" data-a3="${escHtml(displayName(m.from))}" alt="${escHtml(fileName(m.body))}" title="${escHtml(fileName(m.body))}"/>` +
        `<button class="media-dl-btn" title="Download" aria-label="Download" data-act="downloadFile" data-a1="${u}" data-stop>${dlIcon}</button></div>`;
    } else if (m.media_type === 'video') {
      const u = escHtml(m.body).replace(/'/g, '%27');
      bodyHTML = `<div class="media-wrap"><video class="msg-video"${mediaSizeAttr(m.body)} src="${escHtml(mediaSrc(m.body))}" controls preload="metadata"></video>` +
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
        // Placeholder: the body is filled with NODES after the row is inserted
        // into the DOM (see _fillRichBodies below). Assembling the markup as a
        // string is not allowed — the text is written by a user, and one missed
        // escHtml turns into an XSS. Parsing lives in web/js/richtext.js and
        // builds the DOM directly.
        bodyHTML = '<span class="msg-text" data-rich="1"></span>';
        _pendingRich.push([m.id, m.body || '']);
      }
      // Lazy Open Graph preview card under the bubble (filled by linkpreview.js).
      const _lpUrl = (typeof firstUrl === 'function') ? firstUrl(m.body) : null;
      if (_lpUrl && !m._expanded) bodyHTML += `<div class="lp-slot" data-url="${escHtml(_lpUrl)}"></div>`;
    }

    const reactionsHTML = reactionsBarHtml(m);

    // The sender name in a group is printed only on the first message: on a
    // continuation it would repeat itself from the line above.
    const senderHTML = (peer.startsWith('g:') && !isOut && !row.classList.contains('grouped'))
      ? `<div class="msg-sender ${avatarClass(m.from)}-t">${escHtml(m.from)}</div>` : '';
    // The bubble and the reactions go in a wrapper column: the reactions form a
    // SEPARATE row under the bubble, so the bubble itself does not change size
    // when a reaction is added (no "jump"), and nothing is clipped by the
    // bubble's edge or rounding.
    row.innerHTML = `<div class="msg-col"><div class="msg-bubble" data-id="${escHtml(m.id)}" data-act-ctx="showCtxMenu" data-a1="${escHtml(m.id)}" data-a2="${isOut}">${senderHTML}${menuBtn}${fwdBadge}${replyHTML}${bodyHTML}<span class="msg-time">${time}${editedMark}${statusHTML}</span></div>${reactionsHTML}</div>`;
    wrap.appendChild(row);
    _lastRow = row;
  }
  _renderedDay[peer] = lastDayKey;
  if (_animateLast && _lastRow) _lastRow.classList.add('msg-in');
  // We scroll down only if the user was at the bottom anyway (captured before clearing).
  if (wasAtBottom) wrap.scrollTop = wrap.scrollHeight;
  // Remember the intent to hold the bottom — so loading media does not shift the chat.
  wrap._stickBottom = wasAtBottom;
  _fillRichBodies(wrap);
  initScrollDownBtn();
  updateScrollDownBtn();
  // Direct load handlers on each media element (delegation does not work in Safari).
  bindMediaLoadHandlers(wrap);
  // Load the link preview cards (lazily fetched from the server via /unfurl).
  if (typeof hydrateLinkPreviews === 'function') hydrateLinkPreviews(wrap);
}

// ── Markup in the message body ───────────────────────────────────────────
// The [id, source text] buffer is filled while the rows are assembled and parsed
// after insertion into the DOM. No intermediate HTML string ever appears: the
// text is written by a user, and one missed escHtml is an XSS.
const _pendingRich = [];

function _fillRichBodies(wrap) {
  if (!_pendingRich.length) return;
  const render = (typeof renderRichText === 'function') ? renderRichText : null;
  for (const [id, text] of _pendingRich) {
    const bubble = wrap.querySelector('[data-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]');
    const slot = bubble && bubble.querySelector('.msg-text[data-rich="1"]');
    if (!slot) continue;
    slot.removeAttribute('data-rich');
    // Without richtext.js (the file did not load) plain text remains: the node
    // gets textContent, the markup is not parsed, but the message is visible.
    if (render) slot.appendChild(render(text));
    else slot.textContent = text;
  }
  _pendingRich.length = 0;
}

// The reactions bar of one message. An empty string when there are no reactions —
// including when every user is left with empty arrays after removing them
// (previously an empty .reactions-bar was drawn in that case).
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

// A targeted update of the reactions of ONE message: a reaction is the most
// frequent "small" event, while a full renderMessages rebuilds the innerHTML of
// the whole feed (videos blink, the selection is lost). The DOM structure
// repeats renderMessages: .msg-col > [.msg-bubble[data-id], .reactions-bar?].
function updateReactionsBar(peer, msgId) {
  if (peer !== activePeer || !msgId) return;
  const esc = (window.CSS && CSS.escape) ? CSS.escape(msgId) : msgId;
  const bubble = document.querySelector(`.msg-bubble[data-id="${esc}"]`);
  if (!bubble) { renderMessages(peer); return; } // not rendered yet — fallback
  const m = (chats[peer] || []).find(x => x.id === msgId);
  if (!m) return;
  const wrap = document.getElementById('messages-wrap');
  const atBottom = wrap && wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 150;
  const col = bubble.parentElement;
  const existing = col && col.querySelector('.reactions-bar');
  if (existing) existing.remove();
  const html = reactionsBarHtml(m);
  if (html) bubble.insertAdjacentHTML('afterend', html);
  // A bar under the last message changes the height of the feed — we hold the
  // bottom, the way a full renderMessages does.
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
  // Optimistic application: we change it locally at once, and the server will
  // confirm through the "edited" WS event. The fetch is fire-and-forget — on
  // mobile it is often cut off when the keyboard closes (NetworkError) even
  // though the server has already processed the request.
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

// ── The "down" button and the unread counter in an open chat ──────────────
//
// While reading history further up, a person does not see that something new
// arrived at the bottom: the feed does not scroll by itself (that would yank
// them out of their reading), and the message goes unnoticed. The button appears
// only when the distance from the bottom is noticeable — otherwise it would
// flicker on every turn of the wheel.

const SCROLL_DOWN_AT = 220;   // px from the bottom past which the button is needed

function scrollFeedToBottom() {
  const wrap = document.getElementById('messages-wrap');
  if (!wrap) return;
  wrap.scrollTo({ top: wrap.scrollHeight, behavior: 'smooth' });
  wrap._stickBottom = true;
  updateScrollDownBtn();
}

function updateScrollDownBtn() {
  const wrap = document.getElementById('messages-wrap');
  const btn = document.getElementById('scroll-down-btn');
  if (!wrap || !btn) return;
  const gap = wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight;
  const show = gap > SCROLL_DOWN_AT;
  btn.classList.toggle('visible', show);

  // The counter shows the unread messages of the current conversation. While the
  // chat is open and scrolled to the bottom they clear themselves, so the number
  // appears in exactly the case the button was made for.
  const cnt = document.getElementById('scroll-down-count');
  if (!cnt) return;
  const n = (activePeer && typeof countUnread === 'function') ? countUnread(activePeer) : 0;
  if (show && n > 0) { cnt.textContent = n > 99 ? '99+' : n; cnt.style.display = ''; }
  else cnt.style.display = 'none';
}

function initScrollDownBtn() {
  const wrap = document.getElementById('messages-wrap');
  if (!wrap || wrap._sdBound) return;
  wrap._sdBound = true;
  // passive: the handler cancels nothing, and in return the browser does not
  // block scrolling while waiting for preventDefault.
  wrap.addEventListener('scroll', updateScrollDownBtn, { passive: true });
}


// ── Composer: show send only when there is something to send ─────────────
// A class on .input-area rather than on the buttons themselves: switching
// "microphone ↔ send" is a state of the whole input row, and CSS should decide
// it rather than two separate style.display calls in different places.
function updateComposerState() {
  const ta = document.getElementById('msg-textarea');
  const area = document.querySelector('.input-area');
  if (!ta || !area) return;
  area.classList.toggle('has-text', ta.value.trim().length > 0);
}
