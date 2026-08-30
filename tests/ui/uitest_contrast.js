// Readability: a full sweep of the visible text in both themes.
//
// Why this suite exists. web/js/theme.js states the project rule: a theme changes
// ONLY token values, and "as soon as a literal colour appears somewhere it stays
// dark in the light theme — and that gets discovered at the user's end". The rule
// is correct, but it cannot be followed by eye: a literal looks fine in the theme
// it was picked for.
//
// What this suite caught while being written (all of it by measurement, not
// reasoning):
//
//   .hex-modal      contrast 1.06 in the light theme — the background was set as
//                   the literal rgba(22,23,29,.72), bypassing --glass-*. A dark
//                   slab with dark text, six windows at once.
//   .chat-empty-sub 1.76 in the dark theme — the literal #3a3b4a, the caption
//                   barely visible.
//   --muted         4.02 against --bg4 and 4.44 against --bubble-in in the dark
//                   theme, 4.0 and 3.04 in the light one: muted text fell short of
//                   the requirement almost everywhere it was used.
//   --danger-*      the light theme did not redefine them and inherited the dark
//                   ones: "Sign out" gave 3.59 on white.
//
// The threshold is WCAG AA: 4.5 for ordinary text, 3.0 for large text.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : ''));
  if (!ok) failures++;
};
const tag = 'ct' + String(Date.now()).slice(-7);

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

// WebSocket has been built into Node since version 22 — no separate package needed.
const seed = (token, msgs) => new Promise((res, rej) => {
  const s = new WebSocket(BASE.replace('http', 'ws') + '/ws?token=' + token);
  s.onopen = () => {
    msgs.forEach((m, i) => s.send(JSON.stringify({ ...m, id: tag + i })));
    setTimeout(() => { s.close(); res(); }, 700);
  };
  s.onerror = rej;
});

// The sweep runs IN THE PAGE: getComputedStyle is needed for every element.
const SWEEP = `(() => {
  const nums = (c) => (c.match(/[\\d.]+/g) || []).map(Number);
  const over = (fg, bg) => { const a = fg.length > 3 ? fg[3] : 1;
    return [0,1,2].map(i => fg[i]*a + bg[i]*(1-a)); };
  const lum = ([r,g,b]) => { const f=(v)=>{v/=255; return v<=0.03928? v/12.92 : Math.pow((v+0.055)/1.055,2.4);};
    return 0.2126*f(r)+0.7152*f(g)+0.0722*f(b); };
  // The actual background: we collect the WHOLE stack of translucent grounds and
  // composite them bottom-up. Taking the first layer that comes along is not
  // allowed — the dialog glass lies over the scrim, and without compositing you
  // get false findings (the first version of the sweep "saw" a white background in
  // the dark theme).
  const bgOf = (el) => {
    const stack = []; let cur = el;
    while (cur) {
      const c = nums(getComputedStyle(cur).backgroundColor);
      if (c.length >= 3) {
        const a = c.length > 3 ? c[3] : 1;
        if (a > 0) { stack.push([c[0],c[1],c[2],a]); if (a >= 0.999) break; }
      }
      cur = cur.parentElement;
    }
    let base = [0,0,0];
    const html = nums(getComputedStyle(document.documentElement).backgroundColor);
    if (html.length >= 3 && (html.length > 3 ? html[3] : 1) > 0.999) base = html.slice(0,3);
    for (let i = stack.length - 1; i >= 0; i--) base = over(stack[i], base);
    return base;
  };
  const out = [];
  for (const el of document.querySelectorAll('body *')) {
    const cs = getComputedStyle(el);
    if (cs.display === 'none' || cs.visibility === 'hidden' || +cs.opacity < 0.15) continue;
    const r = el.getBoundingClientRect();
    if (r.width < 4 || r.height < 4) continue;
    // Only elements with text OF THEIR OWN: otherwise every container would be
    // counted again for the text of its descendants.
    if (![...el.childNodes].some(n => n.nodeType === 3 && n.textContent.trim())) continue;
    // font-size:0 is the "keep the node, hide the label" technique: that is how the
    // encryption badge is done on a phone, where only the circle with the icon is
    // visible. There is no text on screen, and measuring its contrast is pointless —
    // the first version of the sweep produced a false finding here.
    if (parseFloat(cs.fontSize) < 6) continue;
    const bg = bgOf(el);
    const fg = over(nums(cs.color), bg);
    const L1 = lum(bg), L2 = lum(fg);
    const ratio = (Math.max(L1,L2)+0.05)/(Math.min(L1,L2)+0.05);
    const size = parseFloat(cs.fontSize), bold = +cs.fontWeight >= 700;
    const need = (size >= 24 || (size >= 18.66 && bold)) ? 3.0 : 4.5;
    if (ratio < need) {
      out.push({ sel: el.tagName.toLowerCase() + (el.id ? '#'+el.id : '') +
                      (typeof el.className === 'string' && el.className.trim()
                        ? '.' + el.className.trim().split(/\\s+/).slice(0,2).join('.') : ''),
                 text: el.textContent.trim().slice(0,40),
                 ratio: +ratio.toFixed(2), need, color: cs.color,
                 bg: 'rgb(' + bg.map(Math.round).join(',') + ')' });
    }
  }
  const seen = new Map();
  for (const o of out) { const k = o.sel + '|' + o.color; if (!seen.has(k)) seen.set(k, o); }
  return [...seen.values()].sort((a,b) => a.ratio - b.ratio);
})()`;

(async () => {
  await waitReady();
  const me = 'ann' + tag, bob = 'bob' + tag;
  const tMe = await reg(me); const tBob = await reg(bob);
  await seed(tBob, [{ type: 'message', to: me, body: 'Morning — did you look at the draft?' }]);
  await seed(tMe, [{ type: 'message', to: bob, body: 'Reading it now, second section needs work.' }]);

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  const jsErrors = [];

  for (const theme of ['dark', 'light']) {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
    await ctx.addInitScript((t) => { try { localStorage.setItem('hc_theme', t); } catch {} }, theme);
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push(theme + ': ' + e.message));

    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#auth-screen', { state: 'visible' });
    await page.waitForTimeout(500);

    const applied = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
    check(`the ${theme} theme really was applied`,
          theme === 'light' ? applied === 'light' : applied === null, 'data-theme=' + applied);

    const screens = [['sign-in screen', await page.evaluate(SWEEP)]];

    await page.fill('#auth-username', me);
    await page.fill('#auth-password', 'passw0rd-test');
    await page.click('#auth-btn');
    await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });
    await page.waitForTimeout(1200);
    screens.push(['chat list', await page.evaluate(SWEEP)]);

    const row = page.locator('.contact-item').first();
    if (await row.count()) { await row.click(); await page.waitForTimeout(1200); }
    screens.push(['open chat', await page.evaluate(SWEEP)]);

    await page.evaluate(() => openMyProfile());
    await page.waitForTimeout(600);
    screens.push(['profile', await page.evaluate(SWEEP)]);
    await page.keyboard.press('Escape');
    await page.waitForTimeout(300);

    await page.evaluate(() => open2FASetup());
    await page.waitForTimeout(1400);
    screens.push(['2FA window', await page.evaluate(SWEEP)]);
    await page.evaluate(() => close2FAModal());
    await page.waitForTimeout(300);

    await page.evaluate(() => { if (typeof toggleSettingsMenu === 'function') toggleSettingsMenu(); });
    await page.waitForTimeout(400);
    screens.push(['settings menu', await page.evaluate(SWEEP)]);
    await page.keyboard.press('Escape');
    await page.waitForTimeout(250);

    // ── Places that do not catch the eye during an ordinary look-over ─────
    //
    // The chat list context menu opens on right-click and on a long press — that
    // is, it is invisible until you call it up. And that is where the breakage was
    // found: .ctx-menu set its background as a literal and stayed DARK in the light
    // theme, while its text came from --text and darkened along with the theme.
    const peer = await page.evaluate(() => {
      const el = document.querySelector('.contact-item');
      return el ? el.dataset.peer : null;
    });
    if (peer) {
      await page.click('.contact-item', { button: 'right' });
      await page.waitForTimeout(400);
      screens.push(['chat context menu', await page.evaluate(SWEEP)]);
      await page.keyboard.press('Escape');
      await page.waitForTimeout(250);
    }

    // The message menu is a second hidden place of the same kind.
    const bubble = page.locator('.msg-bubble').first();
    if (await bubble.count()) {
      await bubble.click({ button: 'right' });
      await page.waitForTimeout(400);
      screens.push(['message menu', await page.evaluate(SWEEP)]);
      await page.keyboard.press('Escape');
      await page.waitForTimeout(250);
    }

    // Another person's profile: the email and phone were set to color:#fff and in
    // the light theme simply vanished from the card.
    if (peer) {
      await page.evaluate((u) => { if (typeof openPeerProfile === 'function') openPeerProfile(u); }, peer);
      await page.waitForTimeout(800);
      screens.push(['another person\'s profile', await page.evaluate(SWEEP)]);
      await page.keyboard.press('Escape');
      await page.waitForTimeout(250);
    }

    for (const [name, fn] of [
      ['forwarding', () => { if (typeof openForward === 'function') openForward('x'); }],
      ['new group', () => { if (typeof openGroupModal === 'function') openGroupModal(); }],
      ['network test', () => { if (typeof openNetworkTest === 'function') openNetworkTest(); }],
    ]) {
      try {
        await page.evaluate(fn);
        await page.waitForTimeout(600);
        screens.push([name, await page.evaluate(SWEEP)]);
        await page.keyboard.press('Escape');
        await page.waitForTimeout(250);
      } catch {}
    }

    // An expanded archive — another state nobody looks into.
    try {
      await page.evaluate(() => { if (typeof toggleArchivedView === 'function') toggleArchivedView(); });
      await page.waitForTimeout(500);
      screens.push(['archive expanded', await page.evaluate(SWEEP)]);
      await page.evaluate(() => { if (typeof toggleArchivedView === 'function') toggleArchivedView(); });
      await page.waitForTimeout(300);
    } catch {}

    // The emoji panel and the lightbox: they are opened rarely and so do not catch
    // the eye, while their colours were set separately from everything else.
    try {
      await page.evaluate(() => document.getElementById('emoji-picker')?.classList.add('visible'));
      await page.waitForTimeout(350);
      screens.push(['emoji panel', await page.evaluate(SWEEP)]);
      await page.evaluate(() => document.getElementById('emoji-picker')?.classList.remove('visible'));
    } catch {}

    try {
      await page.evaluate(() => {
        if (typeof openLightbox === 'function')
          openLightbox('data:image/svg+xml,%3Csvg xmlns=%22http://www.w3.org/2000/svg%22 width=%22120%22 height=%2280%22%3E%3C/svg%3E',
                       'Screenshot from the design review');
      });
      await page.waitForTimeout(500);
      screens.push(['image viewer', await page.evaluate(SWEEP)]);
      await page.keyboard.press('Escape');
      await page.waitForTimeout(300);
    } catch {}

    // The attachments panel is a new screen, and it must enter the sweep right
    // away rather than "some day later": that is exactly how literal colours have
    // crept into this project and then broken the light theme.
    try {
      await page.evaluate(() => { if (typeof toggleAttachPanel === 'function') toggleAttachPanel(); });
      await page.waitForTimeout(1400);
      screens.push(['attachments panel', await page.evaluate(SWEEP)]);
      await page.evaluate(() => { if (typeof attachPanelTab === 'function') attachPanelTab('files'); });
      await page.waitForTimeout(900);
      screens.push(['attachments panel — files', await page.evaluate(SWEEP)]);
      await page.evaluate(() => { if (typeof closeAttachPanel === 'function') closeAttachPanel(); });
      await page.waitForTimeout(300);
    } catch {}

    // Message search — another state with markup of its own.
    try {
      await page.fill('#search-input', 'draft');
      await page.waitForTimeout(900);
      screens.push(['search results', await page.evaluate(SWEEP)]);
      await page.fill('#search-input', '');
      await page.waitForTimeout(300);
    } catch {}

    for (const [name, list] of screens) {
      const worst = list.slice(0, 4)
        .map((o) => `${o.sel} ${o.ratio}<${o.need} "${o.text}" ${o.color} on ${o.bg}`).join(' | ');
      check(`${theme}: ${name} — all text readable`, list.length === 0,
            list.length ? `violations ${list.length}: ${worst}` : '');
    }

    // Control: the sweep really did see something. An empty report from an empty
    // sweep would look just like success.
    const seen = await page.evaluate(`(() => {
      let n = 0;
      for (const el of document.querySelectorAll('body *'))
        if ([...el.childNodes].some(x => x.nodeType === 3 && x.textContent.trim())) n++;
      return n; })()`);
    check(`${theme}: the sweep reached the texts (control)`, seen > 20, 'text nodes ' + seen);

    await ctx.close();
  }

  // ══ Phone: it has its own layout and its own rules ═════════════════════
  //
  // On a touch screen some of the rules differ (@media (hover: none)), the list
  // and the chat are separate panels, and the context menu is called up with a
  // long press. Checking only the desktop would mean not checking half the cases.
  for (const theme of ['dark', 'light']) {
    const ctx = await browser.newContext({
      viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true,
      deviceScaleFactor: 2, serviceWorkers: 'block',
    });
    await ctx.addInitScript((t) => { try { localStorage.setItem('hc_theme', t); } catch {} }, theme);
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push('phone/' + theme + ': ' + e.message));

    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#auth-screen', { state: 'visible' });
    await page.fill('#auth-username', me);
    await page.fill('#auth-password', 'passw0rd-test');
    await page.click('#auth-btn');
    await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });
    await page.waitForTimeout(1300);

    const mobile = [['chat list', await page.evaluate(SWEEP)]];
    const row = page.locator('.contact-item').first();
    if (await row.count()) { await row.click(); await page.waitForTimeout(1300); }
    mobile.push(['open chat', await page.evaluate(SWEEP)]);

    await page.evaluate(() => openMyProfile());
    await page.waitForTimeout(700);
    mobile.push(['profile', await page.evaluate(SWEEP)]);
    await page.keyboard.press('Escape');
    await page.waitForTimeout(300);

    await page.evaluate(() => open2FASetup());
    await page.waitForTimeout(1400);
    mobile.push(['2FA window', await page.evaluate(SWEEP)]);
    await page.evaluate(() => { if (typeof close2FAModal === 'function') close2FAModal(); });

    for (const [name, list] of mobile) {
      const worst = list.slice(0, 3)
        .map((o) => `${o.sel} ${o.ratio}<${o.need} "${o.text}" ${o.color} on ${o.bg}`).join(' | ');
      check(`phone/${theme}: ${name} — all text readable`, list.length === 0,
            list.length ? `violations ${list.length}: ${worst}` : '');
    }
    const touch = await page.evaluate(() => matchMedia('(hover: none)').matches);
    check(`phone/${theme}: this really is a touch context (control)`, touch === true);

    await ctx.close();
  }

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
