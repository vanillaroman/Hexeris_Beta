// Hexeris — incremental history sync (ordered by seq).


// sortAndRenderTouched — an intermediate render during a long sync.
// Only the touched conversations are sorted: a full pass over chats on every
// page would turn a linear sync into a quadratic one.
function sortAndRenderTouched(touched) {
  for (const p of touched) sortChat(p);
  if (typeof renderContacts === 'function') renderContacts();
}

async function loadHistory() {
  if (!myUsername || !token) return;
  if (_historyLoading) return;
  _historyLoading = true;
  // The list is drawn IMMEDIATELY, without waiting for a response: before this
  // line, on a first sign-in (when there is no local cache yet) the list was not
  // drawn at all and the person looked at emptiness. The skeleton lives inside
  // renderContacts and is shown for exactly as long as this flag holds.
  if (typeof renderContacts === 'function') renderContacts();
  loadLastSeq();
  try {
    // 1. Instantly show what was saved in localStorage (always readable).
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

    // 2. Incrementally pull ALL new messages from the server, ordered by seq.
    //    The server returns batches of limit; we go forward while data keeps
    //    coming.
    // The global sync cursor. It USED to be taken as max(lastSeqByPeer) — because
    // of that, a device starting a sync from a high seq (or a new one with a
    // partial cache) skipped older messages from OTHER peers:
    // /history?since=<high> simply did not return them. The result — one account
    // had a different contact list and a different amount of history in different
    // browsers. Now the cursor is separate and persistent: a new device starts
    // from 0 and pulls the whole history, not only "newer than what this browser
    // has already seen".
    let globalSince = parseInt(localStorage.getItem(`hc_globalsince_${myUsername}`) || '0', 10);

    const touched = new Set();
    // Page size. The server allows up to 1000 (server/history.go); we used to
    // take 200, so the same history cost five times as many requests — and five
    // times the chance of hitting the limiter (300 requests per window) on the
    // first sign-in, when the whole conversation is downloaded at once.
    const PAGE = 1000;
    let retries429 = 0;
    // WHY THE LOOP IS BUILT THE WAY IT IS.
    //
    // There used to be a page counter here (`while (guard++ < 50)`). With
    // limit=200 that gave a ceiling of 10,000 messages per run, while the
    // globalSince cursor was saved anyway. On a clean browser with a large
    // history the sync silently broke off in the middle, and the remainder was
    // only loaded on the NEXT reload — hence the observed "the contact list
    // arrives in waves after every page refresh".
    //
    // The correct stop condition is not the number of pages but CURSOR PROGRESS.
    // The page counter protected against looping, but paid for it with an
    // incomplete history for the user. Now looping is prevented by the "the
    // cursor did not move" check — it catches the real pathology (the server
    // returns a page but seq does not grow) without limiting the volume.
    // The absolute ceiling is kept as a last line of defence and is deliberately
    // large: 2000 pages of 1000 is two million messages, and no real account ever
    // reaches it.
    // Whether the sync broke off halfway. Any exit from the loop other than
    // "the server returned a partial page" means part of the history did not
    // arrive.
    let incomplete = false;
    let pages = 0;
    let stalled = 0;
    while (pages++ < 2000) {
      const beforeCursor = globalSince;
      const resp = await fetch(`${location.protocol}//${SERVER}/history?since=${globalSince}&limit=${PAGE}`, {
        headers: { 'Authorization': `Bearer ${token}` }
      });
      // A sync cut short halfway is invisible: the history looks complete, just
      // without some of the messages. A 429 is not a data error but a pace: we
      // wait and repeat the same page (the cursor does not move). Other errors
      // stop the loop, but they must be visible in the console rather than
      // silent.
      if (resp.status === 429) {
        // The limiter means "slow down", not "there is no data". Giving up after
        // three attempts is not an option: the user is left with half their
        // contacts and no explanation. We wait longer and try again; six attempts
        // with a growing pause cover the limiter window in full.
        if (retries429++ >= 6) {
          console.warn('history sync: the rate limit did not release within 6 attempts');
          incomplete = true;
          break;
        }
        await new Promise(r => setTimeout(r, Math.min(8000, 1000 * retries429)));
        pages--; // a repeat of the same page does not spend the pass budget
        continue;
      }
      if (!resp.ok) {
        console.warn('history sync: the server answered', resp.status, '— sync interrupted, part of the history was not loaded');
        incomplete = true;
        break;
      }
      const msgs = await resp.json();
      if (!msgs || !msgs.length) break;

      for (const m of msgs) {
        if (m.seq > globalSince) globalSince = m.seq;
        const peer = (m.to && m.to.startsWith('g:')) ? m.to : (m.from === myUsername ? m.to : m.from);
        if (!chats[peer]) chats[peer] = [];

        // Dedup by id.
        const existing = chats[peer].find(x => x.id === m.id);

        // The server already stores/returns the body decrypted (or it is a media URL).
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

      // The cursor did not move even though the server returned something —
      // that is pathological (otherwise the next request returns the same thing
      // and the loop becomes eternal).
      if (globalSince <= beforeCursor) {
        if (++stalled >= 2) {
          console.warn('history sync: the cursor is not moving, stopped to avoid a loop');
          incomplete = true;
          break;
        }
      } else {
        stalled = 0;
      }

      // The contact list is refreshed AS WE GO rather than at the very end. On
      // the first sign-in the sync takes seconds, and all that time the user was
      // looking at an empty sidebar even though the contacts had arrived in the
      // very first batch.
      sortAndRenderTouched(touched);
      // The cursor is saved after EVERY page: if the tab is closed halfway, the
      // next run continues from that point instead of downloading everything
      // again.
      localStorage.setItem(`hc_globalsince_${myUsername}`, String(globalSince));

      if (msgs.length < PAGE) break; // the last batch
    }
    // The pass ceiling triggered — that means the history is longer than one run.
    if (pages > 2000) incomplete = true;
    localStorage.setItem(`hc_globalsince_${myUsername}`, String(globalSince));

    // 3. Sync reactions by their own rseq cursor: a reaction does not move the
    //    message seq, so step 2 does not bring it. The set/unset semantics (not
    //    toggle) make the events idempotent on a repeat sync.
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
          if (!m) continue; // the message is not synced yet — it will come with reactions from /history
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
      // On mobile we do not open a chat automatically — let the user choose.
      // On desktop the sidebar and the chat are visible at once, so restoring is
      // appropriate.
      if (saved && chats[saved] && window.innerWidth > 600) openChat(saved);
    } else {
      renderMessages(activePeer);
    }
    _syncIncomplete = incomplete;
  } catch(e) {
    console.warn('loadHistory error:', e);
    _syncIncomplete = true;
  }
  finally {
    _historyLoading = false;
    scheduleSyncRetry();
    // A repaint after the flag is cleared: otherwise the skeleton would stay on
    // screen forever for someone who genuinely has no conversations — loading
    // finished and there was nobody to tell the list about it.
    if (typeof renderContacts === 'function') renderContacts();
  }
}


// ── Targeted top-up of one conversation ───────────────────
// The main sync is incremental by GLOBAL seq — messages of a group the user has
// just been added to are older than the cursor and get skipped. For such cases
// we pull the history of a specific peer from scratch.
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

// ── Pagination upwards (into the past) ────────────────────
// The client holds the last ~500 messages of a conversation; everything older
// lives only on the server. When the user reaches the top we load a page below
// the smallest loaded seq.

const _loadedAll = {};   // peer -> history exhausted, we stop asking
let _loadingOlder = false;

async function loadOlderMessages(peer) {
  if (_loadingOlder || _loadedAll[peer] || !peer || !token) return false;
  const msgs = chats[peer] || [];
  // The cursor is the smallest seq among the loaded messages. Messages without
  // a seq (not yet confirmed by the server) are ignored.
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
      // localStorage is kept bounded: the last 500 go into the cache, while the
      // depth loaded on demand lives only in the session's memory.
      return true;
    }
    return false;
  } catch (e) {
    return false;
  } finally {
    _loadingOlder = false;
  }
}


// ── Topping up an interrupted sync ────────────────────────────────────────
//
// An interrupted sync used to stay interrupted until the page was reloaded: some
// contacts and messages simply did not appear, and the person never found out —
// the history looked complete, just missing a chunk. The only cure was F5, and
// that is exactly how it was described: "it only arrives after a reload".
//
// Now an incomplete sync tops itself up. The reason for the interruption does
// not matter — the rate limiter, a network failure, a tab closed halfway: in all
// cases the right action is the same.
//
// The attempts are finite and spread out. Hammering a server that answers 429
// means prolonging your own lockout; three attempts with a growing pause cover
// the limiter window, and beyond that the normal path remains — the next socket
// connection will call loadHistory again anyway.
const SYNC_RETRY_DELAYS_MS = [4000, 15000, 45000];

function scheduleSyncRetry() {
  if (!_syncIncomplete) { _syncRetry = 0; return; }
  if (_syncRetry >= SYNC_RETRY_DELAYS_MS.length) {
    console.warn('history sync: history is still incomplete after ' + _syncRetry + ' top-up attempts');
    return;
  }
  const delay = SYNC_RETRY_DELAYS_MS[_syncRetry++];
  clearTimeout(_syncRetryTimer);
  _syncRetryTimer = setTimeout(() => {
    if (!token || !myUsername) return;
    console.info('history sync: topping up the interrupted history (attempt ' + _syncRetry + ')');
    loadHistory();
  }, delay);
}
