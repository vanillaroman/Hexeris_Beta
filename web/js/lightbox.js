// Hexeris — in-app image viewer.
//
// A click on a picture used to call window.open(url) — that is, it took you
// out of the chat into a separate tab with a bare file. In a PWA that is
// especially bad: the app is minimised, you can only come back with the system
// "back" button, and an installed standalone window opens an external browser
// altogether.
//
// Here the overlay sits on top of the app: the chat stays in place, and it
// closes with Esc, a tap on the backdrop, the ✕ button or a swipe down on a
// phone.
//
// Deliberately without zoom and pinch gestures: implementing them properly
// means our own transform matrix and two-finger handling, and the browser
// already does that itself for an image opened via "Download". The job here is
// different — take a quick look and get back to the conversation.

let _lbPrevFocus = null;

function lightboxEl() { return document.getElementById('lightbox'); }

// openLightbox — the image url; caption is optional (file name / sender).
function openLightbox(url, caption) {
  const ov = lightboxEl();
  if (!ov || !url) { window.open(url, '_blank', 'noopener'); return; }

  const img = document.getElementById('lightbox-img');
  const cap = document.getElementById('lightbox-caption');
  const dl  = document.getElementById('lightbox-dl');

  // Show "loading" until the image is ready: without it a slow network leaves
  // the overlay hanging as an empty black rectangle for a second, which looks
  // broken.
  ov.classList.add('loading');
  img.removeAttribute('src');
  img.onload = () => ov.classList.remove('loading');
  img.onerror = () => {
    ov.classList.remove('loading');
    if (typeof toast === 'function') toast('Could not load the image.', 'error');
    closeLightbox();
  };
  img.src = url;
  img.alt = caption || '';

  if (cap) cap.textContent = caption || '';
  if (dl) dl.dataset.a1 = url;

  // Where to return focus after closing — otherwise it drifts to <body> and
  // the next Tab starts walking the page from the beginning.
  _lbPrevFocus = document.activeElement;
  ov.classList.add('open');
  const close = document.getElementById('lightbox-close');
  if (close) close.focus();
}

function closeLightbox() {
  const ov = lightboxEl();
  if (!ov || !ov.classList.contains('open')) return;
  ov.classList.remove('open', 'loading');
  const img = document.getElementById('lightbox-img');
  // Clear src: otherwise the image stays in the tab's memory and the previous
  // one flashes for a moment on the next open.
  if (img) { img.onload = img.onerror = null; img.removeAttribute('src'); }
  if (_lbPrevFocus && document.contains(_lbPrevFocus)) _lbPrevFocus.focus();
  _lbPrevFocus = null;
}

function lightboxOpen() {
  const ov = lightboxEl();
  return !!ov && ov.classList.contains('open');
}

(function initLightbox() {
  // Esc closes. The listener is on document, because focus may be anywhere
  // inside the overlay.
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && lightboxOpen()) { e.preventDefault(); closeLightbox(); }
  });

  // A downward swipe is the familiar way to close a viewer on a phone. The
  // 70px threshold keeps an accidental tremor during a tap from counting.
  const attach = () => {
    const ov = lightboxEl();
    if (!ov) return;
    let startY = null;
    ov.addEventListener('touchstart', (e) => {
      startY = e.touches.length === 1 ? e.touches[0].clientY : null;
    }, { passive: true });
    ov.addEventListener('touchend', (e) => {
      if (startY === null) return;
      const dy = e.changedTouches[0].clientY - startY;
      startY = null;
      if (dy > 70) closeLightbox();
    }, { passive: true });
  };
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', attach);
  } else {
    attach();
  }
})();
