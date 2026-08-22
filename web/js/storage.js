// Hexeris — per-chat message cache in localStorage + addToChat.


function addToChat(peer, msg) {
  if (!chats[peer]) chats[peer] = [];
  const existing = msg.id ? chats[peer].find(m => m.id === msg.id) : null;
  if (existing) {
    // Update an existing entry (for example a seq arriving for one's own message).
    Object.assign(existing, msg);
  } else {
    chats[peer].push(msg);
  }
  bumpSeq(peer, msg.seq);
  sortChat(peer);
  if (msg.body && typeof msg.body === 'string') {
    saveMessageToStorage(peer, msg);
  }
  // If the chat is open the message is read at once and the counter never blinks.
  if (peer === activePeer && typeof markPeerRead === 'function') markPeerRead(peer);
  if (typeof updatePageTitle === 'function') updatePageTitle();
  if (typeof renderContacts === 'function') renderContacts();
}

function saveMessageToStorage(peer, msg) {
  try {
    const key = `hc_msgs_${myUsername}_${peer}`;
    const existing = JSON.parse(localStorage.getItem(key) || '[]');
    const rec = {
      id: msg.id, seq: msg.seq || 0, from: msg.from, to: msg.to, body: msg.body,
      status: msg.status || 'delivered', media_type: msg.media_type || '',
      deleted: msg.deleted || false, edited: msg.edited || false, ts: msg.ts || msg.created_at || Date.now(),
      created_at: msg.created_at || msg.ts || Date.now(),
      reply_to: msg.reply_to || null, forwarded: msg.forwarded || false
    };
    const idx = existing.findIndex(m => m.id === msg.id);
    if (idx >= 0) existing[idx] = { ...existing[idx], ...rec };
    else existing.push(rec);
    // Sort by seq before truncating, so recent messages are not discarded.
    existing.sort((a, b) => (a.seq || 0) - (b.seq || 0) || (a.created_at || a.ts || 0) - (b.created_at || b.ts || 0));
    if (existing.length > 200) existing.splice(0, existing.length - 200);
    localStorage.setItem(key, JSON.stringify(existing));
  } catch {}
}

function loadMessagesFromStorage(peer) {
  try {
    const key = `hc_msgs_${myUsername}_${peer}`;
    return JSON.parse(localStorage.getItem(key) || '[]');
  } catch { return []; }
}

function clearMessagesFromStorage() {
  for (const key of Object.keys(localStorage)) {
    if (key.startsWith(`hc_msgs_`)) localStorage.removeItem(key);
  }
}
