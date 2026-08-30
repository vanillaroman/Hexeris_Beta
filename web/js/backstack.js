// Hexeris — the "back" button closes what is open, not the site.
//
// The problem: the app is a single page and the browser history is empty, so
// the system "back" button on Android and "back" in the browser took you off
// the site straight from an open chat or from the image viewer.
//
// How this works. We do NOT instrument every open and close — there are two
// dozen of them in the project, and every new window would have to be
// remembered. Instead the layers are listed once (top to bottom by priority)
// and watched with a MutationObserver by class/style: a layer opened — we push
// a "stub" into history; a layer closed by itself (Esc, ✕, tap on the backdrop) —
// we take it back. The invariant is simple:
//
//   number of stubs in history == number of open layers
//
// While it holds, "back" closes the top layer exactly once, and when there is
// nothing left to close it leaves the page, as it should.
//
// Only the specific nodes are observed (there are seven), not a subtree: the
// message list is repainted constantly, and a blanket observer on body would
// cost dearly for no reason.

(function () {
  // Layers top to bottom: the one higher in the list closes first.
  // isOpen/close only read state and call functions that already exist.
  const LAYERS = [
    { id: 'lightbox',             open: (el) => el.classList.contains('open'),
      close: () => window.closeLightbox && closeLightbox() },
    { id: 'hex-modal-overlay',    open: (el) => el.classList.contains('open'),
      // Cancel rather than merely hide: the modal returns a promise, and if it
      // is hidden past the button the calling code waits for an answer forever.
      close: () => { const c = document.getElementById('hex-modal-cancel'); if (c) c.click(); } },
    { id: 'forward-overlay',      open: (el) => el.classList.contains('open'),
      close: () => window.closeForward && closeForward() },
    { id: 'peer-profile-overlay', open: (el) => el.classList.contains('open'),
      close: () => window.closePeerProfile && closePeerProfile() },
    { id: 'profile-modal-overlay',open: (el) => el.classList.contains('open'),
      close: () => window.closeMyProfile && closeMyProfile() },
    { id: 'nettest-modal-overlay', open: (el) => el.classList.contains('open'),
      close: () => window.closeNetworkTest && closeNetworkTest() },
    { id: 'group-panel',          open: (el) => el.style.display !== 'none',
      close: () => window.closeGroupPanel && closeGroupPanel() },
    { id: 'chat-search-bar',      open: (el) => el.classList.contains('open'),
      close: () => window.closeChatSearch && closeChatSearch() },
    // #call-overlay is deliberately NOT in the list: "back" is too light a
    // gesture to hang hanging up on. A call is ended with the button.
    // The last layer is an open chat on a phone. On a wide screen the list and
    // the chat are visible at once, there is nothing to close, so it does not count.
    { sel: '.chat-area',          open: (el) => window.innerWidth <= 600 && !el.classList.contains('hidden'),
      close: () => window.showSidebar && showSidebar() },
  ];

  const nodeOf = (l) => l.id ? document.getElementById(l.id) : document.querySelector(l.sel);

  function openLayers() {
    const out = [];
    for (const l of LAYERS) {
      const el = nodeOf(l);
      if (el && l.open(el)) out.push(l);
    }
    return out;
  }

  // There is exactly ONE stub in history no matter how many layers are open,
  // and we NEVER call history.back() ourselves.
  //
  // The previous version kept one stub per layer and removed the extras through
  // history.back(). That was the bug: back() runs asynchronously, while closing
  // a layer is often accompanied by immediately opening another — cancelling a
  // forward confirmation, for instance, closes the dialog and brings the contact
  // list straight back. The pushState for the new layer managed to run BEFORE
  // the browser processed the deferred back(), which then ate the new entry, and
  // the counter drifted away from the real history depth. The next close called
  // back() on the entry BEFORE the app — and threw the user out onto a blank
  // browser page.
  //
  // With a single stub, opening and closing layers does not touch history at
  // all: while at least one layer is open the entry exists; "back" spends it,
  // closes the top layer, and the entry is pushed again if others remain below.
  let guard = false;

  function sync() {
    const open = openLayers().length > 0;
    if (open && !guard) { guard = true; history.pushState({ hcGuard: 1 }, ''); }
    // There is deliberately no inverse operation: an entry can only be removed
    // by navigation, and any navigation here is exactly the mechanism that used
    // to break. The price is one "idle" press of "back" if every layer was
    // closed with the ✕; it is bounded by that one press and does not accumulate.
  }

  window.addEventListener('popstate', () => {
    guard = false;                       // the browser already spent our entry
    const open = openLayers();
    if (!open.length) return;            // nothing to close — we leave the page
    open[0].close();
    // Another layer may remain under the closed one — then we push the entry
    // again so the next "back" closes that one too instead of leaving the site.
    sync();
  });

  function start() {
    const obs = new MutationObserver(sync);
    for (const l of LAYERS) {
      const el = nodeOf(l);
      if (el) obs.observe(el, { attributes: true, attributeFilter: ['class', 'style'] });
    }
    // Window width is part of the condition for .chat-area: rotating the phone
    // or resizing the window on a desktop changes what "a layer is open" means.
    window.addEventListener('resize', sync);
    sync();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
