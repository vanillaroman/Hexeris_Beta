// ── Search ────────────────────────────────────────────────
// The field in the sidebar. While there is a query (2+ characters) the results
// replace the contact list. A click on a result opens the conversation and, if
// the message is in local history, scrolls to it with a highlight (scrollToMsg
// from ui.js). "Show more" keeps scanning deeper through the before=seq
// cursor — the server scans the encrypted history in batches.

let _searchTimer = null;
let _searchNext = 0;      // cursor to continue from; 0 = history exhausted
let _searchQuery = '';

function onSearchInput() {
  clearTimeout(_searchTimer);
  const q = document.getElementById('search-input').value.trim();
  if (q.length < 2) {
    _searchQuery = '';
    document.getElementById('search-results').style.display = 'none';
    document.getElementById('contacts-list').style.display = '';
    return;
  }
  _searchTimer = setTimeout(() => runSearch(q, false), 300);
}

async function runSearch(q, more) {
  const resultsEl = document.getElementById('search-results');
  const listEl = document.getElementById('contacts-list');
  if (!more) {
    _searchQuery = q;
    _searchNext = 0;
    resultsEl.innerHTML = '<div class="search-status">Searching…</div>';
  }
  resultsEl.style.display = '';
  listEl.style.display = 'none';

  try {
    const url = `${location.protocol}//${SERVER}/search?q=${encodeURIComponent(q)}` +
                (more && _searchNext ? `&before=${_searchNext}` : '');
    const resp = await fetch(url, { headers: { 'Authorization': `Bearer ${token}` } });
    if (!resp.ok) throw new Error(resp.status);
    const data = await resp.json();
    if (q !== _searchQuery) return; // the query went stale while the reply flew

    if (!more) resultsEl.innerHTML = '';
    else resultsEl.querySelector('.search-more')?.remove();
    resultsEl.querySelector('.search-status')?.remove();

    for (const h of data.hits) {
      const d = new Date(h.created_at);
      const dateStr = d.toLocaleDateString([], { day: 'numeric', month: 'short' });
      const div = document.createElement('div');
      div.className = 'contact-item search-hit';
      div.onclick = () => openSearchHit(h.peer, h.id);
      // For groups we show the title rather than "g:<id>".
      const isGrp = h.peer.startsWith('g:');
      const name = displayName(h.peer);
      div.innerHTML = `
        <div class="contact-avatar ${avatarClass(h.peer)}${isGrp ? ' is-group' : ''}">${escHtml((name[0] || '?').toUpperCase())}</div>
        <div class="contact-info">
          <div class="contact-name">${escHtml(name)}<span class="search-date">${dateStr}</span></div>
          <div class="contact-preview">${highlightMatch(h.snippet, q)}</div>
        </div>`;
      resultsEl.appendChild(div);
    }

    if (!resultsEl.querySelector('.search-hit')) {
      resultsEl.innerHTML = '<div class="search-status">No messages found</div>';
    }
    if (data.next > 0) {
      _searchNext = data.next;
      const btn = document.createElement('div');
      btn.className = 'search-status search-more';
      btn.textContent = data.hits.length ? 'Show more' : 'Nothing here — search older';
      btn.onclick = () => runSearch(_searchQuery, true);
      resultsEl.appendChild(btn);
    } else {
      _searchNext = 0;
    }
  } catch (e) {
    if (q === _searchQuery) {
      resultsEl.innerHTML = '<div class="search-status">Search failed, try again</div>';
    }
  }
}

// escHtml first, highlighting second — the match is looked up in the already
// escaped text with the same lower case, so <mark> does not break escaping.
function highlightMatch(snippet, q) {
  const esc = escHtml(snippet);
  const qEsc = escHtml(q);
  const i = esc.toLowerCase().indexOf(qEsc.toLowerCase());
  if (i < 0) return esc;
  return esc.slice(0, i) + '<mark>' + esc.slice(i, i + qEsc.length) + '</mark>' + esc.slice(i + qEsc.length);
}

function openSearchHit(peer, msgId) {
  clearSearch();
  openChat(peer);
  // The message may be missing from the local cache (older than the last 500) —
  // then we simply open the conversation without scrolling.
  setTimeout(() => scrollToMsg(msgId), 250);
}

function clearSearch() {
  _searchQuery = '';
  const inp = document.getElementById('search-input');
  if (inp) inp.value = '';
  document.getElementById('search-results').style.display = 'none';
  document.getElementById('contacts-list').style.display = '';
}

// ── Keyboard in search ───────────────────────────────────────────────────
// Ctrl/Cmd+K is already advertised in the markup (the ⌘K badge beside the
// field), but it used to be a label and nothing more: the shortcut did nothing,
// and the results could not be walked with the arrow keys — only with the
// mouse. For a product positioned as fast, that is the main promised gesture
// that did not work.

let _srIndex = -1;   // highlighted result; -1 = none

function _srHits() {
  const el = document.getElementById('search-results');
  return el && el.style.display !== 'none'
    ? Array.from(el.querySelectorAll('.search-hit')) : [];
}

function _srHighlight(hits, i) {
  hits.forEach((h, k) => h.classList.toggle('sr-active', k === i));
  if (hits[i]) hits[i].scrollIntoView({ block: 'nearest' });
}

// focusSearch — put the caret in the field and select the previous query so
// that the next keystrokes replace it right away.
function focusSearch() {
  const inp = document.getElementById('search-input');
  if (!inp) return;
  if (typeof showSidebar === 'function' && window.innerWidth <= 600) showSidebar();
  inp.focus();
  inp.select();
}

function searchKeydown(e) {
  const hits = _srHits();
  if (e.key === 'Escape') {
    const inp = document.getElementById('search-input');
    // The first Esc clears the query, the second drops focus — not the other
    // way round: "clear" is needed more often than "leave the field".
    if (inp && inp.value) { inp.value = ''; onSearchInput(); }
    else if (inp) inp.blur();
    _srIndex = -1;
    return;
  }
  if (!hits.length) return;
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    _srIndex = Math.min(_srIndex + 1, hits.length - 1);
    _srHighlight(hits, _srIndex);
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    _srIndex = Math.max(_srIndex - 1, 0);
    _srHighlight(hits, _srIndex);
  } else if (e.key === 'Enter') {
    e.preventDefault();
    // Enter without a selection opens the first result: that is the most
    // common case — typed, saw the right thing first, pressed Enter.
    (hits[_srIndex] || hits[0]).click();
  }
}

// Typing a new query resets the selection: a highlight left at a position from
// the previous results would point at an unrelated row.
function resetSearchCursor() { _srIndex = -1; }
