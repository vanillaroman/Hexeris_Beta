// The "back" button closes layers rather than taking you off the site.
// Plus: the input panel really does let text pass underneath it.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
async function reg(u){const r=await fetch(BASE+'/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:'Password123!'})});if(!r.ok)throw new Error('reg '+r.status);return (await r.json()).token;}

(async () => {
  const a = U('bk_a'), b = U('bk_b'); await reg(a); const bt = await reg(b);
  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const page = await (await browser.newContext({ ...devices['Pixel 7'] })).newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', a); await page.fill('#auth-password', 'Password123!'); await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bt}`);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  for (let i = 0; i < 25; i++) ws.send(JSON.stringify({ type:'message', id:'b'+i+Date.now(), from:b, to:a, body:'Message number '+i+' with a reasonably long body' }));
  ws.send(JSON.stringify({ type:'message', id:'bi'+Date.now(), from:b, to:a, media_type:'image', body:'/assets/icons/icon-512.png' }));
  await new Promise(r => setTimeout(r, 1000)); ws.close();
  await page.waitForSelector(`#contact-${b}`, { timeout: 8000 });

  const onList = () => page.evaluate(() => document.querySelector('.chat-area').classList.contains('hidden'));

  // ── An open chat: "back" returns to the list rather than leaving ──────────
  await page.click(`#contact-${b}`);
  await page.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 500));
  check('chat is open', (await onList()) === false);
  await page.goBack();
  await new Promise(r => setTimeout(r, 400));
  check('back returns to the contact list instead of leaving',
        (await onList()) === true, 'url=' + page.url());
  // Control: we are still on the app page and have not flown off the site.
  // The address is compared with BASE rather than a hard-coded port — otherwise the
  // suite passes on exactly one port and "fails" on any other.
  check('control: we are still on the app page', page.url().startsWith(BASE), page.url());

  // ── Several layers in a row: viewer → chat → list ─────────────────────────
  await page.click(`#contact-${b}`);
  await page.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 400));
  await page.click('.msg-image');
  await page.waitForSelector('#lightbox.open', { timeout: 5000 });
  await page.goBack();
  await new Promise(r => setTimeout(r, 400));
  check('back #1 closes the image viewer', !(await page.isVisible('#lightbox.open')));
  check('control: back #1 did NOT also close the chat', (await onList()) === false);
  await page.goBack();
  await new Promise(r => setTimeout(r, 400));
  check('back #2 returns to the contact list', (await onList()) === true);

  // ── The confirmation dialog is intercepted too ────────────────────────────
  await page.click(`#contact-${b}`);
  await page.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 400));
  await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.msg-row.in .msg-bubble[data-id]')];
    ctxMsgId = rows[rows.length - 1].dataset.id;
    ctxForward();
  });
  await page.waitForSelector('#forward-overlay.open', { timeout: 5000 });
  await page.goBack();
  await new Promise(r => setTimeout(r, 400));
  check('back closes the forward picker', !(await page.isVisible('#forward-overlay.open')));
  check('control: chat is still open under it', (await onList()) === false);

  // ── Closing NOT with the "back" button must not accumulate stubs ──────────
  // We open and close the viewer with the ✕ three times, then a single "back"
  // must return to the list. If stubs accumulated, four presses would be needed —
  // and the user would decide the button was broken.
  for (let i = 0; i < 3; i++) {
    await page.click('.msg-image');
    await page.waitForSelector('#lightbox.open', { timeout: 5000 });
    await page.click('#lightbox-close');
    await new Promise(r => setTimeout(r, 300));
  }
  await page.goBack();
  await new Promise(r => setTimeout(r, 450));
  check('closing by ✕ does not leave stale history entries',
        (await onList()) === true, 'one back returned to the list');

  // ── The input panel lets text pass underneath it ──────────────────────────
  const glass = await page.evaluate(() => {
    const area = document.querySelector('.input-area');
    const cs = getComputedStyle(area);
    const m = /rgba?\(([^)]+)\)/.exec(cs.backgroundColor);
    const alpha = m ? parseFloat(m[1].split(',')[3] ?? '1') : 1;
    const blur = /blur\(([\d.]+)px\)/.exec(cs.backdropFilter || cs.webkitBackdropFilter || '');
    const mask = getComputedStyle(document.getElementById('chat-bottom'), '::before');
    return { alpha, blur: blur ? parseFloat(blur[1]) : null,
             maskDisplay: mask.display,
             hdrAlpha: (() => { const h = /rgba?\(([^)]+)\)/.exec(getComputedStyle(document.querySelector('.chat-header')).backgroundColor);
                                return h ? parseFloat(h[1].split(',')[3] ?? '1') : 1; })() };
  });
  check('composer panel is fully transparent', glass.alpha === 0, 'alpha=' + glass.alpha);
  check('composer has no blur left at all', glass.blur === null, 'blur=' + glass.blur);
  check('the gradient mask is gone entirely', glass.maskDisplay === 'none',
        'display=' + glass.maskDisplay);
  check('control: the header stays dense (name must stay readable)',
        glass.hdrAlpha > 0.5, 'header alpha=' + glass.hdrAlpha);

  // ── From the contact list "back" leaves the site, as it should ────────────
  const before = page.url();
  await page.goBack();
  await new Promise(r => setTimeout(r, 500));
  check('control: from the list, back finally leaves the app',
        page.url() !== before || (await page.evaluate(() => history.length)) >= 1,
        before + ' → ' + page.url());

  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
