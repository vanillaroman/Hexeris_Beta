// Hexeris — link previews (Open Graph unfurl) + inline URL linkify.
//
// The server does the fetching (with SSRF protection) at /unfurl. Here we:
//   • turn bare URLs in message text into clickable links (linkifyText), and
//   • render a preview card under the bubble, filled lazily from /unfurl.
// Results are cached per-URL in memory; a null result is cached too so a page
// that has no OG tags isn't re-fetched on every re-render.

const _lpCache = {};        // url -> result object | null
const _lpInflight = new Set();

// ═══ WHAT IS EVEN WORTH ASKING ABOUT ══════════════════════════════════════
//
// The client asked the server for a preview of EVERY link in a conversation,
// including the hopeless ones, and repeated that on every page load. In the
// console it looked like dozens of red lines in a row, and on the server like
// just as many trips outside.
//
// And the server refused JUSTLY: a gpg key and a .deb package are not HTML,
// localhost and private addresses are blocked by the SSRF guard, and
// "http://{config.WEB_HOST}" is not an address at all but a piece of config
// from a forwarded snippet. The right place for this check is here: do not go
// where a preview cannot come from.

// Extensions that never hide HTML behind them. The list is deliberately about
// FILES rather than "everything that is not .html": a site without an
// extension in the path is normal.
const _LP_NOT_HTML = new RegExp('\\.(' + [
  'deb','rpm','apk','exe','dmg','msi','iso','img',
  'zip','tar','gz','tgz','bz2','xz','7z','rar',
  'png','jpe?g','gif','webp','svg','ico','bmp','heic','avif',
  'mp4','mov','webm','mkv','avi','mp3','wav','ogg','flac','m4a',
  'pdf','docx?','xlsx?','pptx?','csv','txt','json','xml','yaml','yml',
  'gpg','asc','sig','pem','crt','key','sql','log',
].join('|') + ')$', 'i');

// _lpPreviewable — whether it is worth bothering the server with this link.
//
// Returns false where the answer is predictable in advance. Every branch comes
// from a concrete case in production logs, not from "just in case".
function _lpPreviewable(raw) {
  // Templates from documentation and configs: "{config.WEB_HOST}",
  // "$APP_DOMAIN". These are not addresses, and the server honestly answers 400.
  if (/[{}$]/.test(raw)) return false;
  let u;
  try { u = new URL(raw); } catch (e) { return false; }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return false;
  const host = u.hostname;
  // Trimming trailing punctuation can leave a bare scheme ("https://," becomes
  // "https://"), and such a request went to the server for a 400.
  if (!host) return false;
  // A name without a dot is an internal container or service name: app, db,
  // localhost. That does not resolve outside, and there is no preview there.
  if (host !== 'localhost' && host.indexOf('.') < 0) return false;
  if (host === 'localhost' || host.endsWith('.localhost') || host.endsWith('.local')) return false;
  // Loopback and private ranges — the SSRF guard would reject them anyway.
  if (/^127\./.test(host) || host === '::1' || host === '0.0.0.0') return false;
  if (/^10\./.test(host) || /^192\.168\./.test(host) || /^169\.254\./.test(host)) return false;
  if (/^172\.(1[6-9]|2\d|3[01])\./.test(host)) return false;
  // A file by extension: no HTML behind it, and therefore no Open Graph either.
  if (_LP_NOT_HTML.test(u.pathname)) return false;
  return true;
}

// Refusals survive a reload. Without that every sign-in walked all the links
// of a conversation again: they will not become previewable on their own, while
// the server and the console got the same batch of requests and errors.
const LP_MISS_KEY = 'hc_lp_miss';
const LP_MISS_TTL_MS = 24 * 3600 * 1000;
const LP_MISS_MAX = 400;

function _lpMissLoad() {
  try {
    const raw = JSON.parse(localStorage.getItem(LP_MISS_KEY) || '{}');
    const now = Date.now();
    const out = {};
    for (const k in raw) if (now - raw[k] < LP_MISS_TTL_MS) out[k] = raw[k];
    return out;
  } catch (e) { return {}; }
}
let _lpMiss = _lpMissLoad();

function _lpMissRemember(url) {
  _lpMiss[url] = Date.now();
  // The list is capped: a conversation lives for years, and without a ceiling
  // the record would grow until localStorage refused — and it is shared with
  // the message history.
  const keys = Object.keys(_lpMiss);
  if (keys.length > LP_MISS_MAX) {
    keys.sort((a, b) => _lpMiss[a] - _lpMiss[b]);
    for (const k of keys.slice(0, keys.length - LP_MISS_MAX)) delete _lpMiss[k];
  }
  try { localStorage.setItem(LP_MISS_KEY, JSON.stringify(_lpMiss)); } catch (e) {}
}

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
    // Hopeless from the start, or failed once — we silently remove the slot
    // without touching the server.
    if (!_lpPreviewable(url) || _lpMiss[url]) { _fillPreview(slot, null); return; }
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
        if (!useful) _lpMissRemember(url);
        _fillPreview(slot, useful);
      })
      .catch(() => { _lpCache[url] = null; _lpMissRemember(url); _fillPreview(slot, null); })
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
