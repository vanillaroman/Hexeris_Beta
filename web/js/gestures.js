// Hexeris — gestures and scrolling: swipe to reply, keyboard, media anchor.

// ── Swipe to reply (mobile, TG-style) ────────────────────────────────────────
// A left swipe on a message row activates reply.

function initSwipeToReply() {
  const wrap = document.getElementById('messages-wrap');
  if (!wrap) return;

  const THRESHOLD = 35;
  const MAX_SHIFT = 65;

  let startX = 0, startY = 0;
  let row = null, bubble = null, icon = null;
  let swiping = false, triggered = false;

  function getOrCreateIcon(rowEl) {
    if (rowEl._swipeIcon) return rowEl._swipeIcon;
    const ic = document.createElement('div');
    ic.className = 'swipe-reply-icon';
    ic.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 14 4 9 9 4"/><path d="M20 20v-7a4 4 0 0 0-4-4H4"/></svg>`;
    rowEl.style.position = 'relative';
    rowEl.appendChild(ic);
    rowEl._swipeIcon = ic;
    return ic;
  }

  function resetSwipe() {
    if (bubble) {
      bubble.style.transition = 'transform 0.22s cubic-bezier(.25,.46,.45,.94)';
      bubble.style.transform = '';
    }
    if (icon) {
      icon.style.transition = 'opacity 0.18s, transform 0.18s';
      icon.style.opacity = '0';
      icon.style.transform = 'scale(0.5)';
    }
    row = null; bubble = null; icon = null;
    swiping = false; triggered = false;
  }

  wrap.addEventListener('touchstart', (e) => {
    const touch = e.touches[0];
    startX = touch.clientX;
    startY = touch.clientY;
    const el = document.elementFromPoint(startX, startY);
    if (!el) return;
    row    = el.closest('.msg-row');
    bubble = row?.querySelector('.msg-bubble');
    if (!row || !bubble) { row = null; return; }
    icon = getOrCreateIcon(row);
    icon.style.transition = 'none';
    icon.style.opacity = '0';
    icon.style.transform = 'scale(0.5)';
    swiping = false;
    triggered = false;
  }, { passive: true });

  wrap.addEventListener('touchmove', (e) => {
    if (!row || !bubble) return;
    const touch = e.touches[0];
    const dx = touch.clientX - startX;
    const dy = Math.abs(touch.clientY - startY);
    if (!swiping && dy > Math.abs(dx) + 5) { resetSwipe(); return; }
    if (dx > -8) return;
    e.preventDefault();
    swiping = true;
    const shift = Math.min(Math.abs(dx) * 0.45, MAX_SHIFT);
    const progress = Math.min(shift / THRESHOLD, 1);
    bubble.style.transform = `translateX(-${shift}px)`;
    bubble.style.transition = 'none';
    icon.style.opacity = String(progress);
    icon.style.transform = `scale(${0.5 + 0.5 * progress})`;
    if (shift >= THRESHOLD && !triggered) {
      triggered = true;
      if (navigator.vibrate) navigator.vibrate(30);
    }
  }, { passive: false });

  const onEnd = () => {
    if (!row) return;
    const didTrigger = triggered;
    const msgId = bubble?.dataset?.id;

    // ── Focus immediately, while the touch event is still active ─────────
    // Browsers open the keyboard only when focus() is called synchronously
    // inside a user gesture; calling it later is sometimes blocked.
    if (didTrigger && msgId) {
      const ta = document.getElementById('msg-textarea');
      if (ta) ta.focus();
    }

    resetSwipe();

    if (didTrigger && msgId) {
      const msgs = chats[activePeer] || [];
      const m = msgs.find(x => x.id === msgId);
      if (!m) return;
      if (typeof ctxMsgId !== 'undefined') ctxMsgId = msgId;
      if (typeof ctxReply === 'function') { ctxReply(); return; }
      // Fallback
      if (typeof replyToMsg !== 'undefined') {
        replyToMsg = msgId;
        const text = m.body?.startsWith('/files/') ? '📎 File' : (m.body || '').substring(0, 60);
        const bar = document.getElementById('reply-bar-input');
        const barText = document.getElementById('reply-bar-input-text');
        if (bar && barText) {
          barText.innerHTML = `<strong style="color:var(--accent)">${escHtml(m.from)}</strong>: ${escHtml(text)}`;
          bar.style.display = 'flex';
        }
      }
    }
  };

  wrap.addEventListener('touchend',    onEnd, { passive: true });
  wrap.addEventListener('touchcancel', onEnd, { passive: true });
}

// ── Keyboard-aware scroll (mobile) ───────────────────────────────────────────
// Opening the keyboard pushes the last messages underneath it. The
// visualViewport API knows exactly when the keyboard appears or hides, so the
// feed is scrolled down and the last message stays visible.

function initKeyboardScroll() {
  if (!window.visualViewport) return;

  const wrap = document.getElementById('messages-wrap');
  if (!wrap) return;

  let lastHeight = window.visualViewport.height;

  function isNearBottom() {
    return wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 120;
  }

  // With resizes-content the layout shrinks by itself, so no paddingBottom.
  // Scroll down only if the user was already at the bottom.
  window.visualViewport.addEventListener('resize', () => {
    const newHeight = window.visualViewport.height;
    const keyboardOpened = newHeight < lastHeight - 50;
    lastHeight = newHeight;
    if (keyboardOpened && isNearBottom()) {
      requestAnimationFrame(() => { wrap.scrollTop = wrap.scrollHeight; });
    }
  });

  const ta = document.getElementById('msg-textarea');
  if (ta) {
    ta.addEventListener('focus', () => {
      if (isNearBottom()) {
        setTimeout(() => { wrap.scrollTop = wrap.scrollHeight; }, 200);
      }
    });
  }
}

// ── Floating date badge (TG-style) ───────────────────────────────────────────
// Scrolling the conversation reveals a badge with the current day's date.

function initFloatingDate() {
  const wrap = document.getElementById('messages-wrap');
  if (!wrap) return;

  // Create the badge
  const badge = document.createElement('div');
  badge.className = 'floating-date-badge';
  badge.style.display = 'none';
  wrap.parentElement.insertBefore(badge, wrap);

  let hideTimer = null;

  function showBadge(text) {
    badge.textContent = text;
    badge.style.display = 'block';
    badge.classList.add('visible');
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => {
      badge.classList.remove('visible');
      setTimeout(() => { badge.style.display = 'none'; }, 300);
    }, 1200);
  }

  wrap.addEventListener('scroll', () => {
    // Find the first msg-day still visible or already scrolled past
    const days = wrap.querySelectorAll('.msg-day');
    if (!days.length) return;
    let current = null;
    for (const d of days) {
      const top = d.getBoundingClientRect().top - wrap.getBoundingClientRect().top;
      if (top <= 8) current = d;   // separator above the visible area
    }
    if (current) showBadge(current.textContent);
  }, { passive: true });
}

// ── Media size cache: reserve space so photos/videos don't grow-on-load ───────
// renderMessages() rebuilds the whole list (innerHTML) on every message, so all
// <img>/<video> are recreated and reload from zero height — as they load they
// push the list and the chat "jumps" (Safari/WebKit has no scroll anchoring to
// absorb it; Chrome/Firefox do, which is why only Safari shows it). We remember
// each media's intrinsic w/h (persisted) and emit aspect-ratio on render, so the
// box is reserved immediately and never grows. re-pin (below) is the fallback
// for the very first load, before dimensions are known.
let _mediaDims = null;
function _loadMediaDims() {
  if (_mediaDims) return _mediaDims;
  try { _mediaDims = JSON.parse(localStorage.getItem('hc_media_dims') || '{}'); }
  catch { _mediaDims = {}; }
  return _mediaDims;
}
function mediaSizeAttr(url) {
  const d = _loadMediaDims()[url];       // stored as "w/h"
  if (!d) return '';
  const [w, h] = d.split('/');
  // Intrinsic width/height attributes → the browser (incl. Safari) reserves a
  // correctly-proportioned box before the pixels load; CSS max-width + height:auto
  // keeps it responsive. This is what actually stops the layout jump.
  return ` width="${w}" height="${h}"`;
}
function rememberMediaDims(url, w, h) {
  if (!url || !w || !h) return;
  const m = _loadMediaDims();
  const v = w + '/' + h;
  if (m[url] === v) return;
  m[url] = v;
  try { localStorage.setItem('hc_media_dims', JSON.stringify(m)); } catch {}
}

// ── Keep the chat pinned to the bottom while media loads ─────────────────────
// initMediaScrollAnchor only tracks whether the user is at the bottom (a scroll
// event — fires in every browser). The actual per-media handling is bound
// directly on each element by bindMediaLoadHandlers() (see below), because
// WebKit/Safari does NOT deliver img/video resource `load` events to
// capture-phase listeners on ancestors — so container delegation is a no-op
// there and the whole fix silently died on iPhone/Safari.
function initMediaScrollAnchor() {
  const wrap = document.getElementById('messages-wrap');
  if (!wrap || wrap._mediaAnchorInit) return;
  wrap._mediaAnchorInit = true;
  wrap._stickBottom = true;
  const nearBottom = () => wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight < 150;
  wrap.addEventListener('scroll', () => { wrap._stickBottom = nearBottom(); }, { passive: true });
}

// When one media element learns its size: cache the dims (so future renders
// reserve space and never jump) and, if the user is at the bottom, re-pin.
function _mediaSettled(wrap, el) {
  if (el.tagName === 'IMG') {
    rememberMediaDims(el.getAttribute('src'), el.naturalWidth, el.naturalHeight);
  } else if (el.tagName === 'VIDEO') {
    rememberMediaDims(el.getAttribute('src'), el.videoWidth, el.videoHeight);
  }
  if (!wrap._stickBottom) return;
  wrap.scrollTop = wrap.scrollHeight;
  requestAnimationFrame(() => { if (wrap._stickBottom) wrap.scrollTop = wrap.scrollHeight; });
}

// Bind a DIRECT load handler to every media element in the wrap. Direct element
// listeners fire in all browsers (unlike capture delegation in WebKit). Call
// after each render; already-loaded (cached) media is handled synchronously.
function bindMediaLoadHandlers(wrap) {
  if (!wrap) return;
  wrap.querySelectorAll('img.msg-image, video.msg-video').forEach((el) => {
    if (el._mediaBound) return;
    el._mediaBound = true;
    if (el.tagName === 'IMG') {
      if (el.complete && el.naturalWidth) _mediaSettled(wrap, el);
      else el.addEventListener('load', () => _mediaSettled(wrap, el), { once: true });
    } else { // VIDEO
      if (el.videoWidth) _mediaSettled(wrap, el);
      else el.addEventListener('loadedmetadata', () => _mediaSettled(wrap, el), { once: true });
    }
  });
}

// ── Swipe right from the left edge to leave a chat (iOS-style back) ──────────
// Gated to the left edge so it never conflicts with vertical scroll or the
// swipe-to-reply gesture (which lives on the bubbles and drags left).
function initSwipeToExit() {
  const chatArea = document.querySelector('.chat-area');
  if (!chatArea) return;
  const EDGE = 30;       // start within 30px of the left edge
  const THRESHOLD = 70;  // drag this far right to go back
  let startX = 0, startY = 0, active = false, decided = false, horizontal = false;

  chatArea.addEventListener('touchstart', (e) => {
    if (!isMobile() || e.touches.length !== 1 || e.touches[0].clientX > EDGE) { active = false; return; }
    startX = e.touches[0].clientX; startY = e.touches[0].clientY;
    active = true; decided = false; horizontal = false;
  }, { passive: true });

  chatArea.addEventListener('touchmove', (e) => {
    if (!active) return;
    const dx = e.touches[0].clientX - startX, dy = e.touches[0].clientY - startY;
    if (!decided) {
      if (Math.abs(dx) < 10 && Math.abs(dy) < 10) return;
      horizontal = Math.abs(dx) > Math.abs(dy) && dx > 0;
      decided = true;
      if (!horizontal) { active = false; return; } // vertical → normal scroll
      chatArea.style.transition = 'none';
    }
    if (dx > 0) { e.preventDefault(); chatArea.style.transform = `translateX(${dx}px)`; }
  }, { passive: false });

  const onEnd = (e) => {
    if (!active) return;
    active = false;
    const dx = horizontal ? (e.changedTouches[0].clientX - startX) : 0;
    chatArea.style.transition = '';  // restore the CSS slide
    chatArea.style.transform = '';   // hand back to CSS (.hidden governs)
    if (dx > THRESHOLD) showSidebar();
  };
  chatArea.addEventListener('touchend', onEnd, { passive: true });
  chatArea.addEventListener('touchcancel', onEnd, { passive: true });
}

// Init after DOM ready
// ── Loading older messages when scrolling up ──────────────
// The user reached the top, so the next page into the past is fetched. The
// key detail: inserting content above grows scrollHeight, and without a
// correction the feed would jump. The height is recorded before the insert
// and the position restored relative to it.
function initInfiniteScrollUp() {
  const wrap = document.getElementById('messages-wrap');
  if (!wrap) return;

  let busy = false;
  wrap.addEventListener('scroll', async () => {
    if (busy || wrap.scrollTop > 200 || !activePeer) return;
    if (typeof loadOlderMessages !== 'function') return;
    busy = true;

    const prevHeight = wrap.scrollHeight;
    const prevTop = wrap.scrollTop;
    const added = await loadOlderMessages(activePeer);
    if (added) {
      renderMessages(activePeer);
      // Restore the visual position: the content grew above by exactly
      // (new height - old height), so the scroll moves by the same amount.
      wrap.scrollTop = prevTop + (wrap.scrollHeight - prevHeight);
    }
    busy = false;
  }, { passive: true });
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', () => { initFileInput(); initSwipeToReply(); initSwipeToExit(); initFloatingDate(); initKeyboardScroll(); initMediaScrollAnchor(); initInfiniteScrollUp(); });
} else {
  initFileInput();
  initSwipeToReply();
  initSwipeToExit();
  initFloatingDate();
  initKeyboardScroll();
  initMediaScrollAnchor();
  initInfiniteScrollUp();
}

function expandMsg(id, peer) {
  const m = (chats[peer] || []).find(x => x.id === id);
  if (!m) return;
  m._expanded = true;
  renderMessages(peer);
}
