// Third-round fixes: image viewing, forward confirmation, avatars without a
// caption, the encryption badge, scrolling with the keyboard.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);

let failures = 0;
function check(name, ok, extra) {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + name + (extra ? '  — ' + extra : ''));
  if (!ok) failures++;
}
async function reg(u) {
  const r = await fetch(BASE + '/register', { method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: u, password: 'Password123!' }) });
  if (!r.ok) throw new Error('reg ' + r.status);
  return (await r.json()).token;
}

(async () => {
  const a = U('r3_a'), b = U('r3_b'), c = U('r3_c');
  await reg(a); const bt = await reg(b); await reg(c);

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });

  // ═══ Mobile context ═══
  const page = await (await browser.newContext({ ...devices['Pixel 7'] })).newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', a);
  await page.fill('#auth-password', 'Password123!');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  // Fill the chat: plenty of text (so the feed scrolls) plus an image.
  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bt}`);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  for (let i = 0; i < 40; i++) {
    ws.send(JSON.stringify({ type: 'message', id: 'm' + i + '-' + Date.now(),
      from: b, to: a, body: 'message number ' + i }));
  }
  ws.send(JSON.stringify({ type: 'message', id: 'img-' + Date.now(), from: b, to: a,
    media_type: 'image', body: '/assets/icons/icon-512.png' }));
  await new Promise(r => setTimeout(r, 1200));
  ws.close();
  await page.waitForSelector(`#contact-${b}`, { timeout: 8000 });
  await page.click(`#contact-${b}`);
  await page.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 600));

  // ── 3. The encryption badge: smaller and on one line with its neighbours ──
  const badge = await page.evaluate(() => {
    const e = document.querySelector('.e2e-badge');
    const s = document.querySelector('#chat-search-btn');
    const r = e.getBoundingClientRect(), sr = s.getBoundingClientRect();
    const cs = getComputedStyle(e);
    return { w: +r.width.toFixed(1), h: +r.height.toFixed(1),
             center: +(r.y + r.height / 2).toFixed(1),
             sCenter: +(sr.y + sr.height / 2).toFixed(1),
             bg: cs.backgroundImage, radius: cs.borderRadius };
  });
  check('mobile: lock badge is 26px', badge.w === 26 && badge.h === 26,
        badge.w + '×' + badge.h);
  check('mobile: lock badge is centred with the header icons',
        Math.abs(badge.center - badge.sCenter) <= 1, badge.center + ' vs ' + badge.sCenter);
  check('lock badge has a gradient background',
        /gradient/.test(badge.bg), badge.bg.slice(0, 60));
  check('lock badge is round', badge.radius === '50%', badge.radius);

  // ── 4b. Avatars: no caption and no system image menu ──────────────────────
  const av = await page.evaluate(() => {
    const hdr = document.getElementById('chat-header-avatar');
    const img = document.querySelector('.av-img');
    return {
      hdrTitle: hdr.getAttribute('title'),
      hdrAria: hdr.getAttribute('aria-label'),
      imgFound: !!img,
      imgPointer: img ? getComputedStyle(img).pointerEvents : null,
      imgCallout: img ? getComputedStyle(img).webkitTouchCallout : null,
      imgDraggable: img ? img.getAttribute('draggable') : null,
      titledAvatars: document.querySelectorAll('.contact-avatar[title], .chat-header-avatar[title], .av-img[title], .av-img[alt]:not([alt=""])').length,
    };
  });
  check('avatar has no title tooltip', av.hdrTitle === null, 'title=' + JSON.stringify(av.hdrTitle));
  check('avatar keeps an accessible name', av.hdrAria === 'View profile', av.hdrAria);
  check('no avatar anywhere carries a title/alt label', av.titledAvatars === 0,
        'count=' + av.titledAvatars);

  // ── 5. Viewing an image inside the app ────────────────────────────────────
  const before = page.context().pages().length;
  await page.click('.msg-image');
  await page.waitForSelector('#lightbox.open', { timeout: 5000 });
  check('image opens in an in-app viewer', true);
  check('control: no new tab was opened',
        page.context().pages().length === before, 'pages=' + page.context().pages().length);
  const lb = await page.evaluate(() => {
    const img = document.getElementById('lightbox-img');
    return { src: img.getAttribute('src'), cap: document.getElementById('lightbox-caption').textContent,
             dl: document.getElementById('lightbox-dl').dataset.a1,
             loading: document.getElementById('lightbox').classList.contains('loading') };
  });
  check('viewer shows the right image', /icon-512\.png$/.test(lb.src || ''), lb.src);
  check('viewer names the sender', lb.cap.length > 0, lb.cap);
  check('download button points at the image', lb.dl === lb.src, lb.dl);
  await page.waitForSelector('#lightbox:not(.loading)', { timeout: 5000 });
  check('spinner clears once the image loaded',
        !(await page.evaluate(() => document.getElementById('lightbox').classList.contains('loading'))));

  // A tap on the image itself does NOT close it — otherwise it cannot be examined.
  await page.click('#lightbox-img');
  await new Promise(r => setTimeout(r, 150));
  check('control: tapping the image itself does not close the viewer',
        await page.isVisible('#lightbox.open'));
  // Esc closes it.
  await page.keyboard.press('Escape');
  await new Promise(r => setTimeout(r, 200));
  check('Esc closes the viewer', !(await page.isVisible('#lightbox.open')));
  const cleared = await page.evaluate(() => document.getElementById('lightbox-img').getAttribute('src'));
  check('viewer drops the image on close', cleared === null, 'src=' + cleared);
  // The ✕ button closes it too.
  await page.click('.msg-image');
  await page.waitForSelector('#lightbox.open', { timeout: 5000 });
  await page.click('#lightbox-close');
  await new Promise(r => setTimeout(r, 200));
  check('✕ closes the viewer', !(await page.isVisible('#lightbox.open')));

  // ── 2. Scrolling when the viewport shrinks (a model of the keyboard opening) ─
  // resizes-content shrinks the layout — that is exactly what broke the old
  // "are we at the bottom" check, because it was computed AFTER the shrink.
  await page.evaluate(() => {
    const w = document.getElementById('messages-wrap');
    w.scrollTop = w.scrollHeight;      // pinned to the bottom
  });
  await new Promise(r => setTimeout(r, 200));
  const stuck = await page.evaluate(() => window.feedStuckToBottom);
  check('feed tracks "stuck to bottom" as a flag', stuck === true, 'flag=' + stuck);

  for (let round = 1; round <= 3; round++) {
    await page.setViewportSize({ width: 412, height: 500 });   // "the keyboard opened"
    await new Promise(r => setTimeout(r, 450));
    const gap = await page.evaluate(() => {
      const w = document.getElementById('messages-wrap');
      return Math.round(w.scrollHeight - w.scrollTop - w.clientHeight);
    });
    check('keyboard open #' + round + ': feed still at the bottom', gap <= 8, 'gap=' + gap + 'px');
    await page.setViewportSize({ width: 412, height: 915 });   // "the keyboard closed"
    await new Promise(r => setTimeout(r, 350));
  }

  // The focus → scroll-down path. Viewport emulation does not show a real
  // keyboard, so "shrink the window" proves nothing about the bug: Chrome holds
  // the bottom itself when a container shrinks. The focus branch, though, can be
  // checked, and it is the one responsible for iOS and for a second tap on an
  // already focused field, when no resize arrives at all.
  await page.evaluate(() => {
    const w = document.getElementById('messages-wrap');
    w.scrollTop = w.scrollHeight;
    document.getElementById('msg-textarea').blur();
    w.scrollTop = w.scrollHeight - w.clientHeight - 60;   // 60px up: still "at the bottom"
  });
  await new Promise(r => setTimeout(r, 250));
  await page.evaluate(() => document.getElementById('msg-textarea').focus());
  await new Promise(r => setTimeout(r, 500));
  const afterFocus = await page.evaluate(() => {
    const w = document.getElementById('messages-wrap');
    return Math.round(w.scrollHeight - w.scrollTop - w.clientHeight);
  });
  check('focus on the composer pulls a near-bottom feed to the bottom',
        afterFocus <= 8, 'gap=' + afterFocus + 'px');

  // A negative control for the same branch: if a person is reading an old message
  // far up, focusing the field must not drag them down.
  await page.evaluate(() => {
    const w = document.getElementById('messages-wrap');
    document.getElementById('msg-textarea').blur();
    w.scrollTop = 0;
  });
  await new Promise(r => setTimeout(r, 300));
  await page.evaluate(() => document.getElementById('msg-textarea').focus());
  await new Promise(r => setTimeout(r, 500));
  const afterFocusUp = await page.evaluate(() => document.getElementById('messages-wrap').scrollTop);
  check('negative control: focus does NOT yank a scrolled-up feed down',
        afterFocusUp < 200, 'scrollTop=' + afterFocusUp);

  await page.evaluate(() => {
    const w = document.getElementById('messages-wrap');
    w.scrollTop = w.scrollHeight;
  });
  await new Promise(r => setTimeout(r, 250));

  // Control: if the user has gone UP, shrinking must not yank the feed down.
  await page.evaluate(() => { document.getElementById('messages-wrap').scrollTop = 0; });
  await new Promise(r => setTimeout(r, 250));
  await page.setViewportSize({ width: 412, height: 500 });
  await new Promise(r => setTimeout(r, 450));
  const topGap = await page.evaluate(() => document.getElementById('messages-wrap').scrollTop);
  check('control: scrolled-up feed is NOT yanked to the bottom', topGap < 200, 'scrollTop=' + topGap);
  await page.setViewportSize({ width: 412, height: 915 });
  await new Promise(r => setTimeout(r, 300));

  // ── 4a. Forward confirmation ──────────────────────────────────────────────
  await page.evaluate(() => {
    const w = document.getElementById('messages-wrap');
    w.scrollTop = w.scrollHeight;
  });
  await new Promise(r => setTimeout(r, 300));
  const openForwardFor = async () => {
    await page.evaluate(() => {
      const rows = [...document.querySelectorAll('.msg-row.in .msg-bubble[data-id]')];
      const id = rows[rows.length - 1].dataset.id;
      ctxMsgId = id;      // showCtxMenu needs a real event — we set it directly
      ctxForward();
    });
    await page.waitForSelector('#forward-overlay.open', { timeout: 5000 });
  };
  await openForwardFor();
  check('forward picker opens', true);

  const sentBefore = await page.evaluate(() => Object.keys(chats).length);
  await page.click('#forward-list .fwd-item');
  await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
  const q = await page.evaluate(() => document.getElementById('hex-modal-msg').textContent);
  check('forward asks for confirmation first', /Forward this message to /.test(q), q);
  check('control: the picker got out of the way',
        !(await page.isVisible('#forward-overlay.open')));

  await page.click('#hex-modal-cancel');
  await new Promise(r => setTimeout(r, 250));
  check('Cancel sends nothing',
        (await page.evaluate(() => Object.keys(chats).length)) === sentBefore);
  check('Cancel returns to the picker', await page.isVisible('#forward-overlay.open'));

  await page.click('#forward-list .fwd-item');
  await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
  const outBefore = await page.evaluate(() => document.querySelectorAll('.msg-row.out').length);
  await page.click('#hex-modal-ok');
  await new Promise(r => setTimeout(r, 700));
  const outAfter = await page.evaluate(() => document.querySelectorAll('.msg-row.out').length);
  check('Confirm actually forwards', outAfter > outBefore, outBefore + ' → ' + outAfter);
  check('both overlays are closed afterwards',
        !(await page.isVisible('#forward-overlay.open')) &&
        !(await page.isVisible('#hex-modal-overlay.open')));

  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(2); });
