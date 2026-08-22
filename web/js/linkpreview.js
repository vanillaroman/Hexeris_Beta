// Hexeris — link previews (Open Graph unfurl) + inline URL linkify.
//
// The server does the fetching (with SSRF protection) at /unfurl. Here we:
//   • turn bare URLs in message text into clickable links (linkifyText), and
//   • render a preview card under the bubble, filled lazily from /unfurl.
// Results are cached per-URL in memory; a null result is cached too so a page
// that has no OG tags isn't re-fetched on every re-render.

const _lpCache = {};        // url -> result object | null
const _lpInflight = new Set();

const _URL_RE = /(https?:\/\/[^\s<>"')]+)/gi;

// First http(s) URL in a string, or null.
function firstUrl(text) {
  if (!text) return null;
  const m = text.match(_URL_RE);
  return m ? m[0].replace(/[.,;:!?]+$/, '') : null;
}

// Escape text and wrap any URLs in <a>. Safe: non-URL parts are HTML-escaped,
// URL parts are escaped for use in both href and text.
function linkifyText(text) {
  let out = '';
  let last = 0;
  text.replace(_URL_RE, (match, _u, offset) => {
    out += escHtml(text.slice(last, offset));
    const clean = match.replace(/[.,;:!?]+$/, '');
    const trailing = match.slice(clean.length);
    const safe = escHtml(clean);
    out += `<a class="msg-link" href="${safe}" target="_blank" rel="noopener noreferrer">${safe}</a>${escHtml(trailing)}`;
    last = offset + match.length;
    return match;
  });
  out += escHtml(text.slice(last));
  return out;
}

// Called after renderMessages: fill any empty preview slots.
function hydrateLinkPreviews(wrap) {
  const slots = (wrap || document).querySelectorAll('.lp-slot:not([data-done])');
  slots.forEach(slot => {
    const url = slot.getAttribute('data-url');
    if (!url) return;
    slot.setAttribute('data-done', '1');
    if (url in _lpCache) { _fillPreview(slot, _lpCache[url]); return; }
    if (_lpInflight.has(url)) return;
    _lpInflight.add(url);
    fetch(`${location.protocol}//${SERVER}/unfurl?url=${encodeURIComponent(url)}`, {
      headers: { 'Authorization': 'Bearer ' + token }
    })
      .then(r => r.ok ? r.json() : null)
      .then(res => {
        // Keep only previews that carry something worth showing.
        const useful = res && (res.title || res.description || res.image) ? res : null;
        _lpCache[url] = useful;
        _fillPreview(slot, useful);
      })
      .catch(() => { _lpCache[url] = null; })
      .finally(() => _lpInflight.delete(url));
  });
}

function _fillPreview(slot, res) {
  if (!res) { slot.remove(); return; }
  const img = res.image
    ? `<div class="lp-img" style="background-image:url('${escHtml(res.image).replace(/'/g, '%27')}')"></div>`
    : '';
  const desc = res.description ? `<div class="lp-desc">${escHtml(res.description)}</div>` : '';
  const site = res.site ? `<div class="lp-site">${escHtml(res.site)}</div>` : '';
  slot.innerHTML =
    `<a class="lp-card" href="${escHtml(res.url)}" target="_blank" rel="noopener noreferrer">
       ${img}
       <div class="lp-body">
         ${site}
         <div class="lp-title">${escHtml(res.title || res.url)}</div>
         ${desc}
       </div>
     </a>`;
  // A newly-inserted card can push the last message under the fold; if the user
  // was at the bottom keep them there.
  const wrap = document.getElementById('messages-wrap');
  if (wrap && wrap._stickBottom) wrap.scrollTop = wrap.scrollHeight;
}
