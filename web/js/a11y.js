// Accessibility: the live region and focus behaviour in windows.
//
// ═══ WHAT WAS WRONG ═══════════════════════════════════════════════════════
//
// The client had NOT ONE aria-live region — zero occurrences in the whole
// project. For a messenger that is not decoration: an arriving message, a
// refused send and a change of delivery status were not announced at all, and
// someone using a screen reader simply never learned that they had been
// written to. The only way to notice a new message was to run into it with the
// cursor.
//
// Second: windows opened without taking focus. The screen reader stayed on the
// button that opened the window and read the old screen; closing the window did
// not return focus anywhere either — it fell to <body>, and navigation started
// again from the top of the page.
//
// ═══ SOLUTIONS THAT ARE EASY TO GET WRONG ═════════════════════════════════
//
// • Two regions, not one. polite waits for a pause in speech, assertive
//   interrupts. An arriving message must wait (interrupting someone mid-word is
//   worse than speaking a second later), while "message not sent" must not.
//
// • Announcements are COALESCED. Twenty messages in a row in a group chat, read
//   out one by one, is not accessibility but torture. Everything that arrives
//   within the coalescing window turns into "3 new messages".
//
// • The region is not hidden with display:none or visibility:hidden — that
//   hides it from the screen reader too. What is needed is exactly the "take it
//   off the screen, keep it in the accessibility tree" technique (.sr-only).
//
// • The text goes into an EMPTY region. Writing the same thing into it a second
//   time leaves the reader silent: there is no change. So the region is cleared
//   before writing, and the write itself goes on the next frame.

'use strict';

// Coalescing queue: what has piled up and when we decided to speak.
let _liveQueue = [];
let _liveTimer = null;
// 700 ms is a compromise: shorter and coalescing never kicks in during a fast
// exchange; longer and a single message is announced after a noticeable pause.
const LIVE_COALESCE_MS = 700;

function _liveRegion(kind) {
  const id = 'sr-live-' + kind;
  let el = document.getElementById(id);
  if (!el) {
    el = document.createElement('div');
    el.id = id;
    el.className = 'sr-only';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', kind === 'assertive' ? 'assertive' : 'polite');
    // aria-atomic: read the announcement whole, not only the changed part.
    el.setAttribute('aria-atomic', 'true');
    document.body.appendChild(el);
  }
  return el;
}

// _say puts the text into the region. Clearing plus the next frame is not
// superstition: writing the same text again without clearing does not count as
// a change, and the screen reader skips it.
function _say(kind, text) {
  const el = _liveRegion(kind);
  el.textContent = '';
  requestAnimationFrame(() => { el.textContent = text; });
}

// announce — say it at once, without coalescing. For refusals and confirmations
// of actions: there are few of them, and each relates to something the person
// has just done themselves.
function announce(text, assertive) {
  if (!text) return;
  _say(assertive ? 'assertive' : 'polite', String(text));
}

// announceMessage — announce an incoming message, coalescing the stream.
//
// A screen reader reads more slowly than messages arrive in a busy group chat.
// So either one message is announced in full, or their count: "5 new messages"
// is more useful than five fragments laid over each other.
function announceMessage(from, body) {
  _liveQueue.push({ from: from || '', body: String(body || '') });
  if (_liveTimer) return;
  _liveTimer = setTimeout(() => {
    const batch = _liveQueue;
    _liveQueue = [];
    _liveTimer = null;
    if (!batch.length) return;
    if (batch.length === 1) {
      const m = batch[0];
      // A long message is truncated: the screen reader will read it out in the
      // chat itself, and an announcement must stay an announcement.
      const body = m.body.length > 140 ? m.body.slice(0, 140) + '…' : m.body;
      _say('polite', m.from ? m.from + ': ' + body : body);
      return;
    }
    const senders = [...new Set(batch.map((m) => m.from).filter(Boolean))];
    const who = senders.length === 1 ? ' from ' + senders[0] : '';
    _say('polite', batch.length + ' new messages' + who);
  }, LIVE_COALESCE_MS);
}

// ── Focus in windows ──────────────────────────────────────────────────────
//
// Windows are opened from different places and by different functions (some set
// display:flex inline, others add the .open class). Instead of patching every
// opening point and hoping a new one is not forgotten, we watch the windows
// themselves: became visible — take focus; hidden — give it back. There stays
// one entry point, and a new window gets the behaviour automatically.

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), ' +
                  'select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

let _focusReturn = null;

function _isVisible(el) {
  if (!el) return false;
  const cs = getComputedStyle(el);
  return cs.display !== 'none' && cs.visibility !== 'hidden' && +cs.opacity > 0.01;
}

function _focusInto(overlay) {
  const box = overlay.querySelector('.hex-modal, .group-box, .ctx-menu') || overlay;
  const first = box.querySelector(FOCUSABLE);
  // If there is nothing to focus, focus goes to the window itself. Without
  // tabindex it will not take focus, and without focus the screen reader keeps
  // reading the screen underneath.
  if (first) { try { first.focus(); return; } catch (e) {} }
  if (!box.hasAttribute('tabindex')) box.setAttribute('tabindex', '-1');
  try { box.focus(); } catch (e) {}
}

// Tab must not lead out of an open window onto the elements underneath it: for
// someone who cannot see the screen that looks like falling into nowhere.
function _trapTab(e) {
  if (e.key !== 'Tab') return;
  const overlay = document.querySelector(
    '.hex-modal-overlay.open, .group-overlay[style*="flex"]');
  if (!overlay || !_isVisible(overlay)) return;
  const items = [...overlay.querySelectorAll(FOCUSABLE)].filter(_isVisible);
  if (!items.length) return;
  const first = items[0], last = items[items.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
}

function initA11y() {
  document.addEventListener('keydown', _trapTab, true);

  const overlays = [...document.querySelectorAll('.hex-modal-overlay, .group-overlay')];
  const seen = new WeakMap();
  const obs = new MutationObserver((records) => {
    for (const r of records) {
      const el = r.target;
      const vis = _isVisible(el);
      if (seen.get(el) === vis) continue;
      seen.set(el, vis);
      if (vis) {
        // Where to return focus after closing. Remembered only the first
        // time: windows can open on top of each other (a profile from the
        // menu), and overwriting would lose the original point.
        if (!_focusReturn && document.activeElement && document.activeElement !== document.body) {
          _focusReturn = document.activeElement;
        }
        _focusInto(el);
      } else if (!overlays.some(_isVisible)) {
        // Focus is returned only when the LAST window has closed.
        if (_focusReturn) { try { _focusReturn.focus(); } catch (e) {} }
        _focusReturn = null;
      }
    }
  });
  for (const el of overlays) {
    seen.set(el, _isVisible(el));
    obs.observe(el, { attributes: true, attributeFilter: ['style', 'class'] });
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initA11y);
} else {
  initA11y();
}
