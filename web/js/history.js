// Hexeris — incremental history sync (ordered by seq).


async function loadHistory() {
  if (!myUsername || !token) return;
  if (_historyLoading) return;
  _historyLoading = true;
  loadLastSeq();
  try {
    // 1. Show what is in local storage immediately (always readable).
    for (const key of Object.keys(localStorage)) {
      if (key.startsWith(`hc_msgs_${myUsername}_`)) {
        const peer = key.replace(`hc_msgs_${myUsername}_`, '');
        const stored = loadMessagesFromStorage(peer);
        if (!chats[peer]) chats[peer] = [];
        let updated = false;
        for (const m of stored) {
          if (m.status === 'sending') { m.status = 'sent'; updated = true; }
          if (!chats[peer].find(x => x.id === m.id)) chats[peer].push(m);
          bumpSeq(peer, m.seq);
        }
        sortChat(peer);
        if (updated) {
          try { localStorage.setItem(`hc_msgs_${myUsername}_${peer}`, JSON.stringify(stored)); } catch {}
        }
      }
    }
    renderContacts();

    // 2. Pull every new message from the server incrementally, ordered by
    //    seq, in batches of `limit` until nothing more arrives.
    // The sync cursor is global and persistent. Deriving it from
    // max(lastSeqByPeer) made a device that started high — or a new one with
    // a partial cache — skip older messages from other peers entirely,
    // because /history?since=<high> simply does not return them. The result
    // was one account showing different contact lists and different amounts
    // of history in different browsers. A new device now starts at 0 and
    // pulls the whole history rather than "newer than this browser saw".
    let globalSince = parseInt(localStorage.getItem(`hc_globalsince_${myUsername}`) || '0', 10);

    const touched = new Set();
    let guard = 0;
    let retries429 = 0;
    while (guard++ < 50) {
      const resp = await fetch(`${location.protocol}//${SERVER}/history?since=${globalSince}&limit=200`, {
        headers: { 'Authorization': `Bearer ${token}` }
      });
      // A sync that breaks mid-way is invisible: the history looks complete,
      // merely missing messages. 429 is about pace rather than data, so the
      // same page is retried without moving the cursor. Other errors stop
      // the loop but must be visible in the console rather than silent.
      if (resp.status === 429) {
        if (retries429++ >= 3) {
          console.warn('history sync: rate limited, the rest will load next time');
          break;
        }
        await new Promise(r => setTimeout(r, 1000 * retries429));
        guard--; // retrying the same page does not spend the pass budget
        continue;
      }
      if (!resp.ok) {
        console.warn('history sync: server replied', resp.status, '— sync aborted, history is incomplete');
        break;
      }
      const msgs = await resp.json();
      if (!msgs || !msgs.length) break;

      for (const m of msgs) {
        if (m.seq > globalSince) globalSince = m.seq;
        const peer = (m.to && m.to.startsWith('g:')) ? m.to : (m.from === myUsername ? m.to : m.from);
        if (!chats[peer]) chats[peer] = [];

        // Deduplicate by id.
        const existing = chats[peer].find(x => x.id === m.id);

        // The server stores and serves the body decrypted (or it is a media URL).
        const body = m.deleted ? '[deleted]' : m.body;

        const status = m.read ? 'read' : (m.delivered ? 'delivered' : 'sent');
        const newMsg = {
          id: m.id, seq: m.seq, from: m.from, to: m.to, body,
          status, media_type: m.media_type || '', deleted: !!m.deleted,
          edited: !!m.edited,
          reply_to: m.reply_to || null, forwarded: !!m.forwarded,
          reactions: (m.reactions && Object.keys(m.reactions).length) ? m.reactions : undefined,
          created_at: m.created_at, ts: m.created_at
        };

        if (existing) {
          Object.assign(existing, newMsg);
        } else {
          chats[peer].push(newMsg);
        }
        bumpSeq(peer, m.seq);
        touched.add(peer);
      }

      if (msgs.length < 200) break; // last batch
    }
    localStorage.setItem(`hc_globalsince_${myUsername}`, String(globalSince));

    // 3. Sync reactions on their own rseq cursor: a reaction does not move a
    //    message's seq, so step 2 never brings it. The semantics are set and
    //    unset rather than toggle, which keeps a repeated sync idempotent.
    try {
      let rseq = parseInt(localStorage.getItem(`hc_rseq_${myUsername}`) || '0', 10);
      let rguard = 0;
      while (rguard++ < 20) {
        const rresp = await fetch(`${location.protocol}//${SERVER}/reactions?since=${rseq}`, {
          headers: { 'Authorization': `Bearer ${token}` }
        });
        if (!rresp.ok) break;
        const events = await rresp.json();
        if (!events || !events.length) break;
        for (const e of events) {
          if (e.rseq > rseq) rseq = e.rseq;
          const peer = e.sender === myUsername ? e.recipient : e.sender;
          const m = (chats[peer] || []).find(x => x.id === e.msg_id);
          if (!m) continue; // message not synced yet — it will arrive with its reactions
          if (!m.reactions) m.reactions = {};
          if (!m.reactions[e.from]) m.reactions[e.from] = [];
          const idx = m.reactions[e.from].indexOf(e.emoji);
          if (e.removed && idx !== -1) m.reactions[e.from].splice(idx, 1);
          if (!e.removed && idx === -1) m.reactions[e.from].push(e.emoji);
          touched.add(peer);
        }
        if (events.length < 500) break;
      }
      localStorage.setItem(`hc_rseq_${myUsername}`, String(rseq));
    } catch {}

    if (typeof updatePageTitle === 'function') updatePageTitle();
    for (const p of touched) {
      try {
        localStorage.setItem(`hc_msgs_${myUsername}_${p}`,
          JSON.stringify((chats[p] || []).slice(-500)));
      } catch (e) { console.warn('storage full for', p, e); }
    }
    for (const peer in chats) sortChat(peer);
    renderContacts();
    if (!activePeer) {
      const saved = loadActivePeer();
      // On mobile the chat is not opened automatically — let the user pick.
      // On desktop the sidebar and chat are both visible, so restoring fits.
      if (saved && chats[saved] && window.innerWidth > 600) openChat(saved);
    } else {
      renderMessages(activePeer);
    }
  } catch(e) { console.warn('loadHistory error:', e); }
  finally { _historyLoading = false; }
}


// ── Loading one conversation on demand ────────────────────
// The main sync is incremental by the global seq, so messages of a group the
// user was just added to are older than the cursor and get skipped. For those
// cases the peer's history is pulled from scratch.
async function loadPeerHistory(peer) {
  if (!myUsername || !token) return;
  try {
    let since = 0;
    let guard = 0;
    while (guard++ < 50) {
      const resp = await fetch(`${location.protocol}//${SERVER}/history?peer=${encodeURIComponent(peer)}&since=${since}&limit=200`, {
        headers: { 'Authorization': `Bearer ${token}` }
      });
      if (!resp.ok) break;
      const msgs = await resp.json();
      if (!msgs || !msgs.length) break;
      if (!chats[peer]) chats[peer] = [];
      for (const m of msgs) {
        if (m.seq > since) since = m.seq;
        if (chats[peer].find(x => x.id === m.id)) continue;
        // Keep in sync with the mapping in loadHistory above.
        chats[peer].push({
          id: m.id, seq: m.seq, from: m.from, to: m.to,
          body: m.deleted ? '[deleted]' : m.body,
          status: m.read ? 'read' : (m.delivered ? 'delivered' : 'sent'),
          media_type: m.media_type || '', deleted: !!m.deleted,
          edited: !!m.edited,
          reactions: (m.reactions && Object.keys(m.reactions).length) ? m.reactions : undefined,
          reply_to: m.reply_to || null, forwarded: !!m.forwarded,
          created_at: m.created_at, ts: m.created_at
        });
        bumpSeq(peer, m.seq);
      }
      if (msgs.length < 200) break;
    }
    sortChat(peer);
    try {
      localStorage.setItem(`hc_msgs_${myUsername}_${peer}`,
        JSON.stringify((chats[peer] || []).slice(-500)));
      saveLastSeq();
    } catch (e) {}
    renderContacts();
    if (activePeer === peer) renderMessages(peer);
  } catch (e) {}
}

// ── Paging backwards in time ──────────────────────────────
// The client keeps roughly the last 500 messages of a conversation; anything
// older lives only on the server. When the user scrolls to the top, the page
// below the lowest loaded seq is fetched.

const _loadedAll = {};   // peer -> history exhausted, stop asking
let _loadingOlder = false;

async function loadOlderMessages(peer) {
  if (_loadingOlder || _loadedAll[peer] || !peer || !token) return false;
  const msgs = chats[peer] || [];
  // The cursor is the lowest loaded seq. Messages without one (not yet
  // confirmed by the server) are ignored.
  let minSeq = Infinity;
  for (const m of msgs) if (m.seq && m.seq < minSeq) minSeq = m.seq;
  if (!isFinite(minSeq) || minSeq <= 1) { _loadedAll[peer] = true; return false; }

  _loadingOlder = true;
  try {
    const resp = await fetch(
      `${location.protocol}//${SERVER}/history?peer=${encodeURIComponent(peer)}&before=${minSeq}&limit=100`,
      { headers: { 'Authorization': `Bearer ${token}` } });
    if (!resp.ok) return false;
    const older = await resp.json();
    if (!older || !older.length) { _loadedAll[peer] = true; return false; }

    if (!chats[peer]) chats[peer] = [];
    let added = 0;
    for (const m of older) {
      if (chats[peer].find(x => x.id === m.id)) continue;
      chats[peer].push({
        id: m.id, seq: m.seq, from: m.from, to: m.to,
        body: m.deleted ? '[deleted]' : m.body,
        status: m.read ? 'read' : (m.delivered ? 'delivered' : 'sent'),
        media_type: m.media_type || '', deleted: !!m.deleted,
        edited: !!m.edited,
        reactions: (m.reactions && Object.keys(m.reactions).length) ? m.reactions : undefined,
        reply_to: m.reply_to || null, forwarded: !!m.forwarded,
        created_at: m.created_at, ts: m.created_at
      });
      added++;
    }
    if (older.length < 100) _loadedAll[peer] = true;
    if (added) {
      sortChat(peer);
      // Local storage stays bounded: the cache holds the last 500, and the
      // depth loaded by paging lives only in this session's memory.
      return true;
    }
    return false;
  } catch (e) {
    return false;
  } finally {
    _loadingOlder = false;
  }
}
