// Hexeris — markup in messages.
//
// ── Why a DOM and not an HTML string ────────────────────────────────────────
//
// Markup in a chat is always written by a user, that is, by an untrusted source.
// The usual "build a string and assign innerHTML" route rests on the author
// having escaped everything — and forgetting once in one branch is enough. Here
// nodes are created with createElement and text is put in with textContent, so
// XSS is impossible by construction: a string the browser could parse as markup
// simply never comes into existence.
//
// CSP without script-src 'unsafe-inline' covers some of the vectors, but that is
// the second line of defence, not the first. A messenger is not the place to
// keep only one.
//
// ── What goes into the database ────────────────────────────────────────────
//
// The ORIGINAL text ("**bold**") goes into the database, not the parsed result.
// Otherwise message search would break, exports would become unreadable, and
// body encryption would protect HTML instead of the conversation. Parsing
// happens on the client only, at render time.
//
// ── What is supported and what is deliberately absent ──────────────────────
//
//   ```block```   `code`   **bold**   *italic*   ~~strikethrough~~   > quote
//
// There is no `_italic_` through underscores — deliberately. In this messenger
// people exchange commands and configs where snake_case appears constantly;
// underscores would turn my_var_name into "myvarname" with italics in the
// middle. A character that means something else in the domain cannot be markup.
//
// There are no lists or headings: in a conversation they are barely needed,
// while every construct is one more chance to recognise markup where none was
// intended.
//
// Markup boundaries follow CommonMark rules: an opening character cannot sit
// before a space, and a closing one cannot sit after one. Without that
// "5 * 3 = 15 * 1" would turn into italics, and multiplication comes up in
// conversation more often than italics does.
// IMPORTANT: this is the SOURCE of the regular expression, not a ready object.
// appendInline is recursive (**bold with `code`**), and a RegExp with the /g
// flag keeps lastIndex inside itself: a nested call reset it and the outer loop
// continued from a foreign position — that is, it looped forever and froze the
// tab solid. So every call takes its own instance and there is no shared mutable
// state.
const RT_INLINE_SRC = [
  '`([^`\\n]+)`',                       // 1: `code`
  '\\*\\*(\\S(?:[^*\\n]*\\S)?)\\*\\*',  // 2: **bold**
  '~~(\\S(?:[^~\\n]*\\S)?)~~',          // 3: ~~strikethrough~~
  '\\*(\\S(?:[^*\\n]*\\S)?)\\*',        // 4: *italic*
  '(https?://[^\\s<>"\']+)',            // 5: link
].join('|');

// splitFences — cuts the text into ordinary chunks and ```…``` blocks.
// An unclosed block stays ordinary text: the user is still typing, or simply
// sent three backticks, and "eating" the whole rest of the message in that case
// is not allowed.
function splitFences(text) {
  const parts = [];
  const re = /```([a-zA-Z0-9+#._-]{0,20})?[ \t]*\n?([\s\S]*?)```/g;
  let last = 0, m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) parts.push({ code: false, text: text.slice(last, m.index) });
    parts.push({ code: true, lang: m[1] || '', text: m[2].replace(/\n$/, '') });
    last = m.index + m[0].length;
  }
  if (last < text.length) parts.push({ code: false, text: text.slice(last) });
  return parts;
}

// rtCodeBlock — a monospace block with a "copy" button. The </> button in the
// composer was made for exactly this scenario: commands and configs are
// forwarded to be run, not to be read.
function rtCodeBlock(code, lang) {
  const box = document.createElement('div');
  box.className = 'md-codebox';

  const head = document.createElement('div');
  head.className = 'md-codehead';
  const label = document.createElement('span');
  label.className = 'md-codelang';
  label.textContent = lang || 'code';
  head.appendChild(label);

  const btn = document.createElement('button');
  btn.className = 'md-copy';
  btn.type = 'button';
  btn.textContent = 'Copy';
  // The text is taken from the closure rather than from the DOM: in the node it
  // could have picked up line breaks inserted by the layout.
  btn.addEventListener('click', (e) => {
    e.stopPropagation();
    const done = () => { btn.textContent = 'Copied'; setTimeout(() => { btn.textContent = 'Copy'; }, 1400); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(code).then(done).catch(() => {});
    }
  });
  head.appendChild(btn);

  const pre = document.createElement('pre');
  pre.className = 'md-code';
  const el = document.createElement('code');
  el.textContent = code;
  pre.appendChild(el);

  box.appendChild(head);
  box.appendChild(pre);

  // ── The "this line is longer, it can be scrolled" hint ───────────────────
  // Styling the scrollbar itself is not enough: on macOS, iOS and Android it is
  // an overlay — it takes no space and is invisible at rest. The recipient of a
  // long command saw a truncated line and had no idea it continued. So we draw
  // the edges explicitly: a class on the container turns on a gradient on the
  // right/left rather than relying on the system bar.
  const sync = () => {
    const max = pre.scrollWidth - pre.clientWidth;
    box.classList.toggle('has-right', max > 1 && pre.scrollLeft < max - 1);
    box.classList.toggle('has-left', pre.scrollLeft > 1);
  };
  pre.addEventListener('scroll', sync, { passive: true });
  // Width cannot be measured before insertion into the document — we compute it on the next frame.
  requestAnimationFrame(sync);
  // The monospace font loads separately and changes the line width only after
  // the first frame; without a second check the hint would be left over from the
  // old sizes. document.fonts is not everywhere — hence the check.
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(sync).catch(() => {});

  return box;
}

// appendInline — parses one chunk of plain text into nodes.
function appendInline(parent, text) {
  let last = 0, m;
  const re = new RegExp(RT_INLINE_SRC, 'g');   // its own instance per call
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) parent.appendChild(document.createTextNode(text.slice(last, m.index)));
    let node;
    if (m[1] !== undefined) {
      node = document.createElement('code');
      node.className = 'md-inline-code';
      node.textContent = m[1];
    } else if (m[2] !== undefined) {
      node = document.createElement('strong');
      appendInline(node, m[2]);            // nesting: **bold with `code`**
    } else if (m[3] !== undefined) {
      node = document.createElement('s');
      appendInline(node, m[3]);
    } else if (m[4] !== undefined) {
      node = document.createElement('em');
      appendInline(node, m[4]);
    } else {
      // A link. Trailing punctuation is not part of the address: in
      // "see http://a/b." the dot ends the sentence, it is not part of the URL.
      const raw = m[5];
      const clean = raw.replace(/[.,;:!?)\]]+$/, '');
      node = document.createElement('a');
      node.className = 'msg-link';
      node.href = clean;
      node.target = '_blank';
      node.rel = 'noopener noreferrer';
      node.textContent = clean;
      if (raw.length > clean.length) {
        parent.appendChild(node);
        parent.appendChild(document.createTextNode(raw.slice(clean.length)));
        last = m.index + m[0].length;
        continue;
      }
    }
    parent.appendChild(node);
    last = m.index + m[0].length;
  }
  if (last < text.length) parent.appendChild(document.createTextNode(text.slice(last)));
}

// appendQuotes — lines starting with "> " are gathered into a quote block.
// Consecutive ones are joined into a single quote, as in any markdown.
function appendQuotes(parent, text) {
  const lines = text.split('\n');
  let buf = [];
  const flushQuote = () => {
    if (!buf.length) return;
    const q = document.createElement('blockquote');
    q.className = 'md-quote';
    appendInline(q, buf.join('\n'));
    parent.appendChild(q);
    buf = [];
  };
  let plain = [];
  const flushPlain = () => {
    if (!plain.length) return;
    appendInline(parent, plain.join('\n'));
    plain = [];
  };
  for (const line of lines) {
    const q = /^>\s?(.*)$/.exec(line);
    if (q) { flushPlain(); buf.push(q[1]); }
    else   { flushQuote(); plain.push(line); }
  }
  flushQuote();
  flushPlain();
}

// renderRichText — the main entry point. Returns a fragment ready to be
// inserted. Line breaks do not become <br>: the container is declared
// white-space: pre-wrap and the text is kept as is — including the indentation
// inside commands that <br> would lose.
function renderRichText(text) {
  const frag = document.createDocumentFragment();
  for (const part of splitFences(String(text == null ? '' : text))) {
    if (part.code) frag.appendChild(rtCodeBlock(part.text, part.lang));
    else appendQuotes(frag, part.text);
  }
  return frag;
}

// rtHasMarkup — whether the text contains anything we would parse. Needed where
// it matters not to spend work on plain text (the chat-list preview).
function rtHasMarkup(text) {
  return /```|`[^`\n]+`|\*\*\S|~~\S|^\s*>\s/m.test(String(text || ''));
}

// rtStripMarkup — text without markup, for the list preview and notifications.
// There a short human description is needed, not "**Done** — see `log`".
function rtStripMarkup(text) {
  return String(text || '')
    .replace(/```[a-zA-Z0-9+#._-]*\n?([\s\S]*?)```/g, '$1')
    .replace(/`([^`\n]+)`/g, '$1')
    .replace(/\*\*(\S(?:[^*\n]*\S)?)\*\*/g, '$1')
    .replace(/~~(\S(?:[^~\n]*\S)?)~~/g, '$1')
    .replace(/\*(\S(?:[^*\n]*\S)?)\*/g, '$1')
    .replace(/^>\s?/gm, '')
    .replace(/\s+/g, ' ')
    .trim();
}

// ── "Send as code" mode ─────────────────────────────────────────────────────
//
// This is NOT a "with formatting / without" switch. In this messenger people
// forward commands and configs, and the action they need there is exactly one:
// "send it as is, monospaced, with a copy button". The button is named after
// precisely that.
//
// The state lives until one message is sent and then resets: a sticky mode
// means the next ordinary sentence goes out as a code block.

let _codeMode = false;

function codeModeOn() { return _codeMode; }

function setCodeMode(on) {
  _codeMode = !!on;
  const btn = document.getElementById('fmt-code-btn');
  if (btn) {
    btn.classList.toggle('active', _codeMode);
    btn.setAttribute('aria-pressed', _codeMode ? 'true' : 'false');
    btn.title = _codeMode ? 'Sending as code block — click to turn off' : 'Send as code block';
  }
  const ta = document.getElementById('msg-textarea');
  if (ta) ta.placeholder = _codeMode ? 'Code…' : 'Message...';
}

function toggleCodeMode() {
  setCodeMode(!_codeMode);
  // Focus is returned to the field only where there is a real cursor. On a
  // phone focus() raises the on-screen keyboard, and pressing "send as code"
  // threw it up over half the screen every time — even though the person might
  // simply be marking the mode in advance with no intention of typing yet.
  const fine = window.matchMedia && window.matchMedia('(hover: hover) and (pointer: fine)').matches;
  if (!fine) return;
  const ta = document.getElementById('msg-textarea');
  if (ta) ta.focus();
}

// wrapSelection — Ctrl+B / Ctrl+I wrap the selection. When there is no
// selection we insert the pair of characters and put the caret between them:
// the person pressed "bold" to carry on typing in bold, not to get "****".
function wrapSelection(marker) {
  const ta = document.getElementById('msg-textarea');
  if (!ta) return;
  const s = ta.selectionStart, e = ta.selectionEnd;
  const val = ta.value;
  const sel = val.slice(s, e);
  ta.value = val.slice(0, s) + marker + sel + marker + val.slice(e);
  if (sel) ta.setSelectionRange(s + marker.length, e + marker.length);
  else     ta.setSelectionRange(s + marker.length, s + marker.length);
  ta.focus();
  if (typeof autoResize === 'function') autoResize(ta);
}
