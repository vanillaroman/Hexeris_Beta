// The conversation attachments panel: Media / Files / Voice.
//
// ═══ WHERE IT LIVES AND WHY ══════════════════════════════════════════════
//
// The panel replaces the feed inside the same conversation rather than popping
// up as a separate window or gathering media from all chats at once. The reason
// is simple: a file is looked for where it was sent — "Lena sent the mockup last
// week". A shared screen with everything a person has ever received solves that
// task worse, while multiplying the cost of an accidental screen share on a call.
//
// ═══ WHAT IS CHEAP HERE AND WHAT IS NOT ══════════════════════════════════
//
// Filtering by type is done by the server: media_type is a plaintext column, so
// it is an indexed query. The file name lies inside the encrypted body, so the
// server cannot search by it except by decrypting the whole history (that is how
// the general search works: up to 20,000 messages per request).
//
// Hence the honest limit of the "Filter by name" field: it filters WHAT IS
// ALREADY LOADED, and it is labelled exactly so. Promising a full search by name
// and quietly searching one page is worse than not promising: the person will
// conclude the file is not there.

'use strict';

let _apOpen = false;
let _apKind = 'media';
let _apNext = 0;         // cursor of the next page (0 — no more)
let _apItems = [];       // what has been loaded for the current tab
let _apPeer = null;      // the conversation the loaded data belongs to
let _apLoading = false;
// Deferred skeleton. A tab response usually arrives within tens of
// milliseconds, and an instant skeleton manages to flash and vanish — the eye
// reads that as a jerk rather than as loading. We show it only if the response
// takes longer than the threshold; if it arrives in time, the content swaps
// straight away.
let _apSkTimer = null;
const AP_SKELETON_DELAY_MS = 180;

const AP_LABEL = { media: 'Media', files: 'Files', voice: 'Voice' };

function _apEl(id) { return document.getElementById(id); }

// The file name is taken from the SHARED helper in helpers.js: on disk the file
// sits under a random hash, and the client puts the real name into the body's
// #fragment (see upload.js). A local copy of that logic already existed here and
// had drifted from the original — it read the path and showed the hash. One
// place for the whole app, otherwise the file name in the feed and in the panel
// will one day differ again.
function _apName(url) {
  return (typeof fileName === 'function') ? fileName(url) : String(url || '');
}

function _apExt(url) {
  const n = _apName(url);
  const i = n.lastIndexOf('.');
  return i > 0 ? n.slice(i + 1).toLowerCase() : '';
}

function toggleAttachPanel() {
  if (_apOpen) { closeAttachPanel(); return; }
  if (typeof activePeer === 'undefined' || !activePeer) return;
  _apOpen = true;
  const panel = _apEl('attach-panel');
  const wrap = _apEl('messages-wrap');
  if (panel) panel.hidden = false;
  if (wrap) wrap.style.display = 'none';
  const btn = _apEl('attach-panel-btn');
  if (btn) { btn.classList.add('active'); btn.setAttribute('aria-expanded', 'true'); }
  // Changing conversation resets what was loaded: showing the previous peer's
  // attachments under the current header is plain misinformation.
  if (_apPeer !== activePeer) { _apItems = []; _apNext = 0; _apPeer = activePeer; }
  loadAttachments(true);
}

function closeAttachPanel() {
  _apOpen = false;
  const panel = _apEl('attach-panel');
  const wrap = _apEl('messages-wrap');
  if (panel) panel.hidden = true;
  if (wrap) wrap.style.display = '';
  const btn = _apEl('attach-panel-btn');
  if (btn) { btn.classList.remove('active'); btn.setAttribute('aria-expanded', 'false'); }
}

function attachPanelTab(kind) {
  if (!AP_LABEL[kind] || kind === _apKind) return;
  _apKind = kind;
  _apItems = []; _apNext = 0;
  document.querySelectorAll('.ap-tab').forEach((t) => {
    const on = t.dataset.a1 === kind;
    t.classList.toggle('active', on);
    t.setAttribute('aria-selected', on ? 'true' : 'false');
  });
  loadAttachments(true);
}

async function loadAttachments(reset) {
  if (_apLoading) return;
  const peer = (typeof activePeer !== 'undefined') ? activePeer : null;
  if (!peer) return;
  _apLoading = true;
  // The previous content is NOT wiped: it stays on screen until the new one
  // arrives. An empty panel where the list was is exactly the flicker.
  clearTimeout(_apSkTimer);
  if (reset) {
    const kindAtStart = _apKind;
    _apSkTimer = setTimeout(() => {
      if (_apLoading && _apKind === kindAtStart) _apRender('loading');
    }, AP_SKELETON_DELAY_MS);
  }
  try {
    const qs = new URLSearchParams({ peer, kind: _apKind });
    if (!reset && _apNext) qs.set('before', String(_apNext));
    const r = await fetch(`${location.protocol}//${SERVER}/attachments?` + qs.toString(),
                          { headers: { 'Authorization': 'Bearer ' + token } });
    if (!r.ok) { _apRender('error'); return; }
    const data = await r.json();
    _apItems = reset ? (data.items || []) : _apItems.concat(data.items || []);
    _apNext = data.next || 0;
    _apRender();
  } catch (e) {
    _apRender('error');
  } finally {
    _apLoading = false;
    clearTimeout(_apSkTimer);
  }
}

// attachPanelFollowPeer — the conversation changed while the panel is open.
//
// Called from openChat. The panel does not close: the person opened it
// deliberately and switches peers precisely because of the attachments. But
// showing the previous peer's files while doing so is not allowed — under
// someone else's name in the header that is plain misinformation, not "the data
// is slightly stale".
function attachPanelFollowPeer(peer) {
  if (!_apOpen || !peer || peer === _apPeer) return;
  _apPeer = peer;
  _apItems = []; _apNext = 0;
  // The filter applied to the previous conversation — keeping it means showing
  // an empty panel and leaving the person to guess why.
  const f = _apEl('ap-filter');
  if (f) f.value = '';
  loadAttachments(true);
}

function _apFiltered() {
  const q = (_apEl('ap-filter') || {}).value || '';
  const s = q.trim().toLowerCase();
  if (!s) return _apItems;
  return _apItems.filter((it) => _apName(it.url).toLowerCase().includes(s));
}

function _apRender(state) {
  const body = _apEl('ap-body');
  if (!body) return;

  if (state === 'loading') {
    // The skeleton repeats the geometry of the tiles, so filling in the data
    // does not shift the panel content.
    body.innerHTML = '<div class="ap-grid" aria-hidden="true">' +
      '<div class="ap-sk"></div>'.repeat(_apKind === 'media' ? 9 : 4) + '</div>' +
      '<div class="sr-only">Loading attachments</div>';
    return;
  }
  if (state === 'error') {
    body.innerHTML = '<div class="ap-empty">Could not load attachments. ' +
                     'Check your connection and try again.</div>';
    return;
  }

  const items = _apFiltered();
  if (!items.length) {
    const filtering = ((_apEl('ap-filter') || {}).value || '').trim().length > 0;
    body.innerHTML = '<div class="ap-empty">' +
      (filtering
        ? 'Nothing loaded matches that name.<br><span>The filter only covers what is already loaded — scroll down to load more.</span>'
        : 'No ' + AP_LABEL[_apKind].toLowerCase() + ' in this chat yet.') +
      '</div>';
    return;
  }

  let html;
  if (_apKind === 'media') {
    html = '<div class="ap-grid">' + items.map((it, i) => {
      const url = escHtml(it.url);
      const name = escHtml(_apName(it.url));
      const cap = '<span class="ap-cap">' + name + '</span>';
      if (it.media_type === 'video') {
        return '<button class="ap-cell ap-video" data-act="apOpen" data-a1="' + i + '" ' +
               'title="' + name + '" aria-label="Video ' + name + ' from ' + escHtml(it.from) + '">' +
               '<video src="' + url + '" preload="metadata" muted playsinline></video>' +
               '<span class="ap-play" aria-hidden="true">▶</span>' + cap + '</button>';
      }
      // loading="lazy" is not decoration here: without it, opening the tab
      // pulls all sixty images at once.
      return '<button class="ap-cell" data-act="apOpen" data-a1="' + i + '" ' +
             'title="' + name + '" aria-label="Image ' + name + ' from ' + escHtml(it.from) + '">' +
             '<img src="' + url + '" alt="' + name + '" loading="lazy" decoding="async"/>' + cap + '</button>';
    }).join('') + '</div>';
  } else {
    html = '<div class="ap-list">' + items.map((it, i) =>
      '<button class="ap-row" data-act="apOpen" data-a1="' + i + '">' +
      '<span class="ap-ico">' + escHtml((_apExt(it.url) || '?').slice(0, 4)) + '</span>' +
      '<span class="ap-meta"><span class="ap-fname">' + escHtml(_apName(it.url)) + '</span>' +
      '<span class="ap-sub">' + escHtml(it.from) + ' · ' +
      (typeof contactTime === 'function' ? escHtml(contactTime(it.created_at)) : '') +
      '</span></span></button>').join('') + '</div>';
  }

  if (_apNext) {
    html += '<button class="ap-more" data-act="apMore">Load older</button>';
  }
  body.innerHTML = html;
  body._items = items;
  _apFadeInMedia(body);
}

// _apFadeInMedia — until an image loads the browser draws an empty frame with
// a "no image" icon, and when the panel opens that is visible as a flash of
// empty icons. We hide the media until the load event and reveal it on that;
// the tile background stays the placeholder. The handler is attached from code
// rather than as an onload attribute: inline handlers are forbidden by
// Content-Security-Policy.
function _apFadeInMedia(root) {
  for (const el of root.querySelectorAll('.ap-cell img, .ap-cell video')) {
    // An image from cache is ready already — waiting for the event is
    // pointless, it will not come and the tile would stay transparent forever.
    if (el.tagName === 'IMG' && el.complete && el.naturalWidth > 0) {
      el.classList.add('ap-ready');
      continue;
    }
    const done = () => el.classList.add('ap-ready');
    el.addEventListener('load', done, { once: true });
    el.addEventListener('loadeddata', done, { once: true });
    // A broken file must not leave a hole: we show a tile with a caption.
    el.addEventListener('error', () => {
      el.classList.add('ap-ready');
      el.closest('.ap-cell')?.classList.add('ap-broken');
    }, { once: true });
  }
}

// apOpen — open an attachment. Images go into the EXISTING lightbox: writing a
// separate viewer for the panel would mean a second gallery with different key
// behaviour and a different way to close.
function apOpen(idx) {
  const body = _apEl('ap-body');
  const items = (body && body._items) || [];
  const it = items[+idx];
  if (!it) return;
  if (it.media_type === 'image' && typeof openLightbox === 'function') {
    openLightbox(it.url, _apName(it.url) + ' · ' + it.from);
    return;
  }
  window.open(it.url, '_blank', 'noopener');
}

function apMore() { loadAttachments(false); }

document.addEventListener('input', (e) => {
  if (e.target && e.target.id === 'ap-filter') _apRender();
});
