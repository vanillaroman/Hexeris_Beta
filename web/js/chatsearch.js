// Hexeris — search inside the open conversation.
// It searches the loaded messages of the active chat, highlighting matches
// and stepping through them with ↑/↓. Deep server-side search across the
// whole history lives separately, in the sidebar (search.js).

let _chatSearchMatches = [];
let _chatSearchIdx = -1;

function toggleChatSearch() {
  const bar = document.getElementById('chat-search-bar');
  if (!bar || !activePeer) return;
  if (bar.classList.contains('open')) { closeChatSearch(); return; }
  bar.classList.add('open');
  const inp = document.getElementById('chat-search-input');
  inp.value = '';
  document.getElementById('chat-search-count').textContent = '';
  _chatSearchMatches = []; _chatSearchIdx = -1;
  setTimeout(() => inp.focus(), 30);
}

function closeChatSearch() {
  const bar = document.getElementById('chat-search-bar');
  if (bar) bar.classList.remove('open');
  _chatSearchMatches = []; _chatSearchIdx = -1;
  clearChatSearchHighlight();
}

function onChatSearchInput() {
  const q = document.getElementById('chat-search-input').value.trim().toLowerCase();
  const countEl = document.getElementById('chat-search-count');
  _chatSearchMatches = []; _chatSearchIdx = -1;
  clearChatSearchHighlight();
  if (!q || !activePeer) { countEl.textContent = ''; return; }
  for (const m of (chats[activePeer] || [])) {
    if (m.deleted || !m.body) continue;
    // Files match on their name, everything else on its text.
    const hay = (m.media_type ? (m.body.split('/').pop() || '') : m.body).toLowerCase();
    if (hay.includes(q)) _chatSearchMatches.push(m.id);
  }
  if (!_chatSearchMatches.length) { countEl.textContent = '0/0'; return; }
  _chatSearchIdx = _chatSearchMatches.length - 1; // start from the most recent
  gotoChatMatch();
}

function chatSearchStep(dir) {
  if (!_chatSearchMatches.length) return;
  _chatSearchIdx = (_chatSearchIdx + dir + _chatSearchMatches.length) % _chatSearchMatches.length;
  gotoChatMatch();
}

function gotoChatMatch() {
  document.getElementById('chat-search-count').textContent =
    (_chatSearchIdx + 1) + '/' + _chatSearchMatches.length;
  clearChatSearchHighlight();
  const id = _chatSearchMatches[_chatSearchIdx];
  const el = document.querySelector(`[data-id="${id}"]`);
  if (el) {
    el.classList.add('csb-current');
    el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }
}

function clearChatSearchHighlight() {
  document.querySelectorAll('.msg-bubble.csb-current').forEach(el => el.classList.remove('csb-current'));
}

function onChatSearchKey(e) {
  if (e.key === 'Enter') { e.preventDefault(); chatSearchStep(e.shiftKey ? -1 : 1); }
  else if (e.key === 'Escape') { e.preventDefault(); closeChatSearch(); }
}
