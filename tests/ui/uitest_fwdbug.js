// The bug from the report: forward → tap a contact → cancel → tap again →
// confirm → the app throws you out onto a blank browser page.
//
// We check not "it did not crash" but exactly what broke: the history length and
// that the page is still ours. Plus a series of repeats — the bug did not show up
// the first time, only after the stub counter drifted away from the history.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
async function reg(u){const r=await fetch(BASE+'/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:'Password123!'})});if(!r.ok)throw new Error('reg '+r.status);return (await r.json()).token;}

async function run(label, deviceOpts) {
  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const a = U('fb_a'), b = U('fb_b'), c = U('fb_c');
  await reg(a); const bt = await reg(b); await reg(c);
  const page = await (await browser.newContext(deviceOpts)).newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', a); await page.fill('#auth-password', 'Password123!'); await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bt}`);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  for (let i = 0; i < 5; i++) ws.send(JSON.stringify({ type:'message', id:'f'+i+Date.now(), from:b, to:a, body:'message '+i }));
  await new Promise(r => setTimeout(r, 800)); ws.close();
  await page.waitForSelector(`#contact-${b}`, { timeout: 10000 });
  await page.click(`#contact-${b}`);
  await page.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 500));

  const openPicker = async () => {
    await page.evaluate(() => {
      const rows = [...document.querySelectorAll('.msg-row.in .msg-bubble[data-id]')];
      ctxMsgId = rows[rows.length - 1].dataset.id;
      ctxForward();
    });
    await page.waitForSelector('#forward-overlay.open', { timeout: 5000 });
  };
  const alive = () => /127\.0\.0\.1:8766/.test(page.url()) &&
    page.evaluate(() => !!document.getElementById('chat-screen'));

  console.log('  ── ' + label + ' ──');

  // ── The exact sequence from the report ──────────────────────────────────
  await openPicker();
  await page.click('#forward-list .fwd-item');           // tap on a contact
  await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
  await page.click('#hex-modal-cancel');                 // cancel
  await new Promise(r => setTimeout(r, 400));
  check(label + ': picker comes back after cancel',
        await page.isVisible('#forward-overlay.open'));

  await page.click('#forward-list .fwd-item');           // tap again
  await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
  const outBefore = await page.evaluate(() => document.querySelectorAll('.msg-row.out').length);
  await page.click('#hex-modal-ok');                     // confirm
  await new Promise(r => setTimeout(r, 900));

  check(label + ': still on the app after confirm (NOT thrown out)',
        await alive(), 'url=' + page.url());
  const outAfter = await page.evaluate(() => document.querySelectorAll('.msg-row.out').length);
  check(label + ': the message was actually forwarded', outAfter > outBefore,
        outBefore + ' → ' + outAfter);
  check(label + ': both overlays closed',
        !(await page.isVisible('#forward-overlay.open')) &&
        !(await page.isVisible('#hex-modal-overlay.open')));

  // ── Repeat the cycle three times: the bug accumulated ────────────────────
  for (let i = 1; i <= 3; i++) {
    await openPicker();
    await page.click('#forward-list .fwd-item');
    await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
    await page.click('#hex-modal-cancel');
    await new Promise(r => setTimeout(r, 300));
    await page.click('#forward-list .fwd-item');
    await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
    await page.click('#hex-modal-ok');
    await new Promise(r => setTimeout(r, 700));
    check(label + ': cycle #' + i + ' — still on the app', await alive(), 'url=' + page.url());
  }

  // Cancelling several times in a row, without confirming at all.
  await openPicker();
  for (let i = 0; i < 3; i++) {
    await page.click('#forward-list .fwd-item');
    await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
    await page.click('#hex-modal-cancel');
    await new Promise(r => setTimeout(r, 300));
  }
  check(label + ': three cancels in a row keep the app alive', await alive(), 'url=' + page.url());
  await page.click('#forward-overlay .hex-modal-actions .hex-btn');   // Cancel the whole modal
  await new Promise(r => setTimeout(r, 300));


  // ── A message fingerprint must depend on the CONTENT ────────────────────
  // An edit of the same length on an already edited message used to give the same
  // fingerprint — the new text simply never appeared on screen.
  const fp = await page.evaluate(() => {
    const peer = document.getElementById('messages-wrap').dataset.peer;
    const m = chats[peer].find(x => x.from === peer && !x.deleted);
    m.body = 'the server went down!'; m.edited = true;
    renderMessages(peer);
    const first = [...document.querySelectorAll('.msg-text')].some(n => n.textContent.includes('the server went down!'));
    // A second edit: the same length, edited is already set.
    m.body = 'the server came back!';
    renderMessages(peer);
    const second = [...document.querySelectorAll('.msg-text')].some(n => n.textContent.includes('the server came back!'));
    return { first, second, sameLength: 'the server went down!'.length === 'the server came back!'.length,
             lens: ['the server went down!'.length, 'the server came back!'.length] };
  });
  check(label + ': control — the lengths really do match', fp.sameLength === true);
  check(label + ': first edit shows up', fp.first === true);
  check(label + ': a same-length second edit ALSO shows up', fp.second === true, JSON.stringify(fp));

  // ── New layers: in-chat search and the network test close with "back" ────
  await page.evaluate(() => toggleChatSearch());
  await new Promise(r => setTimeout(r, 300));
  check(label + ': chat search opened', await page.isVisible('#chat-search-bar.open'));
  await page.goBack();
  await new Promise(r => setTimeout(r, 400));
  check(label + ': back closes the in-chat search',
        !(await page.isVisible('#chat-search-bar.open')) && /127\.0\.0\.1:8766/.test(page.url()),
        'url=' + page.url());

  // ── The queue is read once per render, not once per message ──────────────
  const reads = await page.evaluate(() => {
    const peer = document.getElementById('messages-wrap').dataset.peer;
    const key = 'hc_pending_' + myUsername;
    let n = 0;
    const orig = Storage.prototype.getItem;
    Storage.prototype.getItem = function (k) { if (k === key) n++; return orig.call(this, k); };
    // A full rebuild: change a message so the fingerprint diverges.
    chats[peer][0].body = 'otherwise ' + Math.random();
    renderMessages(peer);
    Storage.prototype.getItem = orig;
    return { reads: n, rows: document.querySelectorAll('.msg-row').length };
  });
  check(label + ': outbox is read once per render, not per message',
        reads.reads <= 2, 'reads ' + reads.reads + ' for ' + reads.rows + ' rows');

  // ── After all this fuss "back" must still behave correctly ───────────────
  const isMobile = !!(deviceOpts && deviceOpts.isMobile);
  if (isMobile) {
    const onList = () => page.evaluate(() => document.querySelector('.chat-area').classList.contains('hidden'));
    check(label + ': chat still open before back', (await onList()) === false);
    await page.goBack();
    await new Promise(r => setTimeout(r, 500));
    check(label + ': back returns to the contact list, not off-site',
          (await onList()) === true && /127\.0\.0\.1:8766/.test(page.url()), 'url=' + page.url());
  }

  await browser.close();
}

(async () => {
  await run('mobile', { ...devices['Pixel 7'] });
  await run('desktop', { viewport: { width: 1280, height: 800 } });
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
