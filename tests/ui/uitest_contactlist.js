// A contact row: the presence indicator, the right-hand column, section order.
//
// All three checks were written from concrete breakages, not "just in case":
//
//   1. The presence dot jumped up on repeated clicks on a contact.
//      The live presence update replaces className WHOLESALE, and along with the
//      state it wiped the class that positioned the dot.
//   2. On a phone the date was covered by the pin button: there is no hover
//      there, the button stayed visible always, and the time was hidden only on
//      :hover.
//   3. The expanded archive ended up at the very bottom of the list — apart from
//      its heading, which sits at the top.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
const tag = 'cl' + String(Date.now()).slice(-7);

async function waitReady() {
  for (let i = 0; i < 40; i++) {
    try { if ((await fetch(BASE + '/')).ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error('instance not ready at ' + BASE);
}

const reg = async (u) => {
  const r = await fetch(BASE + '/register', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: u, password: 'passw0rd-test' }),
  });
  if (!r.ok) throw new Error('register ' + u + ': ' + r.status);
  return (await r.json()).token;
};

// We use the WebSocket built into Node — it has been global since version 22.
// The ws module used to be looked up next to playwright, and where the package is
// missing the whole suite failed: in a full run that looked like a layout
// breakage even though it was the environment that broke.
const seed = (token, msgs) => new Promise((res, rej) => {
  const s = new WebSocket(BASE.replace('http', 'ws') + '/ws?token=' + token);
  s.onopen = () => {
    msgs.forEach((m, i) => s.send(JSON.stringify({ ...m, id: tag + '-' + Math.random().toString(36).slice(2) + i })));
    setTimeout(() => { s.close(); res(); }, 600);
  };
  s.onerror = rej;
});

(async () => {
  await waitReady();
  const me = 'me' + tag, bob = 'bob' + tag, kim = 'kim' + tag;
  const tMe = await reg(me); await reg(bob); await reg(kim);
  await seed(tMe, [
    { type: 'message', to: bob, body: 'first note to bob' },
    { type: 'message', to: kim, body: 'first note to kim' },
  ]);

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
  const page = await ctx.newPage();
  const jsErrors = [];
  page.on('pageerror', (e) => jsErrors.push(e.message));

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#auth-screen', { state: 'visible' });
  await page.fill('#auth-username', me);
  await page.fill('#auth-password', 'passw0rd-test');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });
  await page.waitForSelector(`#contact-${bob}`, { timeout: 10000 });

  const dotBox = () => page.evaluate((p) => {
    const d = document.getElementById('dot-' + p);
    if (!d) return null;
    const r = d.getBoundingClientRect();
    const av = d.closest('.contact-av').getBoundingClientRect();
    return { pos: getComputedStyle(d).position,
             dx: Math.round(r.left - av.left), dy: Math.round(r.top - av.top),
             cls: d.className };
  }, bob);

  // ── 1. The presence dot survives a class overwrite ───────────────────────
  const before = await dotBox();
  check('the dot is positioned absolutely', before && before.pos === 'absolute', JSON.stringify(before));

  // Exactly what the live presence update does: the whole className.
  await page.evaluate((p) => {
    onlineStatuses[p] = true;
    const d = document.getElementById('dot-' + p);
    d.className = presenceDotClass(p);
  }, bob);
  const afterOnline = await dotBox();
  check('after className was overwritten the dot did NOT move',
        afterOnline && afterOnline.pos === 'absolute' &&
        afterOnline.dx === before.dx && afterOnline.dy === before.dy,
        JSON.stringify({ before, afterOnline }));

  // And back to offline — a second pass along the same path.
  await page.evaluate((p) => {
    onlineStatuses[p] = false;
    const d = document.getElementById('dot-' + p);
    d.className = presenceDotClass(p);
  }, bob);
  const afterOffline = await dotBox();
  check('and on the way back to offline it is in place too',
        afterOffline && afterOffline.pos === 'absolute' &&
        afterOffline.dx === before.dx && afterOffline.dy === before.dy,
        JSON.stringify(afterOffline));

  // Repeated clicks on the contact — the original scenario from the complaint.
  for (let i = 0; i < 4; i++) { await page.click(`#contact-${bob}`); await page.waitForTimeout(150); }
  const afterClicks = await dotBox();
  check('the dot is in place after four clicks on the contact',
        afterClicks && afterClicks.pos === 'absolute' &&
        afterClicks.dx === before.dx && afterClicks.dy === before.dy,
        JSON.stringify(afterClicks));

  // ── 2. Phone: the pin button does not sit over the time ──────────────────
  //
  // A REAL mobile context is needed (isMobile + hasTouch) rather than faking the
  // media feature through CDP: the first attempt to do it with
  // Emulation.setEmulatedMedia silently did not work (matchMedia('(hover:none)')
  // stayed false), and the check "passed" for a false reason — the cursor had
  // simply been moved off the row, so :hover did not fire. That is, the test was
  // measuring a mouse, not a touch screen.
  {
    const mctx = await browser.newContext({
      viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true,
      deviceScaleFactor: 3, serviceWorkers: 'block',
    });
    const mp = await mctx.newPage();
    mp.on('pageerror', (e) => jsErrors.push('mobile: ' + e.message));
    await mp.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await mp.waitForSelector('#auth-screen', { state: 'visible' });
    await mp.fill('#auth-username', me);
    await mp.fill('#auth-password', 'passw0rd-test');
    await mp.click('#auth-btn');
    await mp.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });
    await mp.waitForSelector(`#contact-${bob}`, { timeout: 10000 });

    const mq = await mp.evaluate(() => matchMedia('(hover: none)').matches);
    check('the mobile context really has no hover', mq === true,
          'matchMedia(hover:none)=' + mq);

    const touch = await mp.evaluate((p) => {
      const row = document.getElementById('contact-' + p);
      const pin = row.querySelector('.contact-pin');
      const time = row.querySelector('.contact-time');
      const vis = (el) => !!el && getComputedStyle(el).display !== 'none' &&
                          getComputedStyle(el).visibility !== 'hidden' &&
                          parseFloat(getComputedStyle(el).opacity) > 0.01;
      const overlap = (a, b) => {
        if (!vis(a) || !vis(b)) return false;
        const ra = a.getBoundingClientRect(), rb = b.getBoundingClientRect();
        return !(ra.right <= rb.left || ra.left >= rb.right || ra.bottom <= rb.top || ra.top >= rb.bottom);
      };
      return { pinVisible: vis(pin), timeVisible: vis(time),
               timeText: time ? time.textContent.trim() : '',
               overlapping: overlap(pin, time) };
    }, bob);

    check('the time is visible on a phone', touch.timeVisible && touch.timeText.length > 0, JSON.stringify(touch));
    check('on a phone the pin button does NOT cover the time', !touch.overlapping, JSON.stringify(touch));
    // Pinning on a phone has not gone anywhere — it is in the long-press context
    // menu. The button was removed deliberately, not lost.
    const hasCtxPin = await mp.evaluate(() => !!document.getElementById('cctx-pin'));
    check('pinning is available from the context menu', hasCtxPin === true);

    await mctx.close();
  }

  // ── 3. The archive sits right under its heading, not at the end of the list ─
  await page.evaluate((p) => toggleChatArchive(p), kim);
  await page.waitForTimeout(600);
  // Expand the section.
  await page.evaluate(() => toggleArchivedView());
  await page.waitForTimeout(400);

  const order = await page.evaluate(() => {
    const out = [];
    for (const el of document.getElementById('contacts-list').children) {
      if (el.classList.contains('archive-row')) out.push({ kind: 'archive-row' });
      else if (el.classList.contains('list-section')) out.push({ kind: 'section', text: el.textContent });
      else if (el.classList.contains('contact-item')) {
        out.push({ kind: 'chat', peer: el.dataset.peer, archived: el.classList.contains('archived') });
      }
    }
    return out;
  });

  const firstArchivedIdx = order.findIndex(o => o.kind === 'chat' && o.archived);
  const firstNormalIdx = order.findIndex(o => o.kind === 'chat' && !o.archived);
  check('the expanded archive comes ABOVE the ordinary chats',
        firstArchivedIdx >= 0 && firstNormalIdx >= 0 && firstArchivedIdx < firstNormalIdx,
        JSON.stringify(order));
  check('the "Archived" row comes before its contents',
        order[0] && order[0].kind === 'archive-row', JSON.stringify(order[0]));

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
