// Hexeris — global state, session/peer/seq tracking, sort utils.
// Note: there is no client-side cryptography here — message bodies are
// delivered to the client already decrypted by the server (server-held
// AES-256-GCM key, at rest). See index.html's encryption-info popup for
// the actual (honest) data-protection model.

// Same-origin: in production the client is served from the backend host
// (location.host === chat.example.com), so behavior is identical there. In
// local iPhone/PWA testing the client is served from the cloudflared tunnel
// (collo.rncn8n.com) whose dev server reverse-proxies API + /ws to the backend,
// avoiding the backend's missing CORS headers. Hardcoding the backend host here
// breaks that proxy path ("cannot connect to the server").
const SERVER = location.host;
let token = null;
let myUsername = null;
let ws = null;
let activePeer = null;
let chats = {};
let authMode = 'login';

const onlineStatuses = {}; // peer → true/false

const scrollPositions = {}; // scroll position of each chat

// lastSeq[peer] — the highest seq we have already loaded for this peer.
// Stored per-user in localStorage for incremental top-ups.
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

// Sorting messages in a chat: by seq when present, otherwise by created_at/ts.
function sortChat(peer) {
  if (!chats[peer]) return;
  chats[peer].sort((a, b) => {
    const sa = a.seq || 0, sb = b.seq || 0;
    if (sa && sb && sa !== sb) return sa - sb;
    const ta = a.created_at || a.ts || 0, tb = b.created_at || b.ts || 0;
    return ta - tb;
  });
}

let _historyLoading = false; // guard against parallel loadHistory calls
// Whether the last history sync was cut short and how many times it has been
// topped up since. These live here rather than in history.js: the top-up
// scheduler reads them from finally, and keeping the state next to the rest of
// the application state is more honest.
let _syncIncomplete = false;
let _syncRetry = 0;
let _syncRetryTimer = null;
