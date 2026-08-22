// Hexeris — global state, session/peer/seq tracking, sort utils.
// Note: there is no client-side cryptography here — message bodies are
// delivered to the client already decrypted by the server (server-held
// AES-256-GCM key, at rest). See index.html's encryption-info popup for
// the actual (honest) data-protection model.

// Same origin by design: the client is served by the backend host, so the
// API and /ws live at location.host. Hard-coding a backend host here breaks
// any setup where the client is served through a proxy or tunnel — the
// symptom is "cannot connect to the server".
const SERVER = location.host;
let token = null;
let myUsername = null;
let ws = null;
let activePeer = null;
let chats = {};
let authMode = 'login';

const onlineStatuses = {}; // peer → true/false

const scrollPositions = {}; // scroll position per chat

// lastSeq[peer] is the highest seq already loaded for that peer, kept per
// user in local storage for incremental catch-up.
let lastSeqByPeer = {};
function loadLastSeq() {
  try { lastSeqByPeer = JSON.parse(localStorage.getItem('hc_lastseq_' + myUsername) || '{}'); }
  catch { lastSeqByPeer = {}; }
}
function saveLastSeq() {
  try { localStorage.setItem('hc_lastseq_' + myUsername, JSON.stringify(lastSeqByPeer)); } catch {}
}
function bumpSeq(peer, seq) {
  if (!seq) return;
  if (!lastSeqByPeer[peer] || seq > lastSeqByPeer[peer]) {
    lastSeqByPeer[peer] = seq;
    saveLastSeq();
  }
}

// Message ordering in a chat: by seq when present, otherwise by timestamp.
function sortChat(peer) {
  if (!chats[peer]) return;
  chats[peer].sort((a, b) => {
    const sa = a.seq || 0, sb = b.seq || 0;
    if (sa && sb && sa !== sb) return sa - sb;
    const ta = a.created_at || a.ts || 0, tb = b.created_at || b.ts || 0;
    return ta - tb;
  });
}

let _historyLoading = false; // guards against parallel loadHistory calls
