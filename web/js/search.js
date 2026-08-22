// ── Search ────────────────────────────────────────────────
// The sidebar field. While a query of two or more characters is present, the
// results replace the contact list. Clicking a result opens the conversation
// and, when the message is in local history, scrolls to it with a highlight.
// "Show more" continues the scan into the past through a before=seq cursor,
// with the server scanning the encrypted history in portions.

let _searchTimer = null;
let _searchNext = 0;      // continuation cursor; 0 = history exhausted
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
    if (q !== _searchQuery) return; // the query changed while the answer flew

    if (!more) resultsEl.innerHTML = '';
    else resultsEl.querySelector('.search-more')?.remove();
    resultsEl.querySelector('.search-status')?.remove();

    for (const h of data.hits) {
      const d = new Date(h.created_at);
      const dateStr = d.toLocaleDateString([], { day: 'numeric', month: 'short' });
      const div = document.createElement('div');
      div.className = 'contact-item search-hit';
      div.onclick = () => openSearchHit(h.peer, h.id);
      // Groups are shown by name rather than as "g:<id>".
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

// Escaping first, highlighting second: the match is found in the already
// escaped text with the same lower-casing, so <mark> cannot break escaping.
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
  // The message may be missing from the local cache (older than the last
  // 500), in which case the conversation opens without scrolling.
  setTimeout(() => scrollToMsg(msgId), 250);
}

function clearSearch() {
  _searchQuery = '';
  const inp = document.getElementById('search-input');
  if (inp) inp.value = '';
  document.getElementById('search-results').style.display = 'none';
  document.getElementById('contacts-list').style.display = '';
}
