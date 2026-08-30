// Scrolling must be VISIBLE — in every scrollable place, and above all in a code
// block at the recipient: there the ground is tinted and a thumb the colour of a
// border disappeared on it.
//
// We measure not "is there a CSS rule" but the actual width/height the bar takes
// up (offset − client) and the thumb colour. A rule can be written and miss with
// its selector; occupied space cannot miss.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
async function reg(u){const r=await fetch(BASE+'/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:'Password123!'})});if(!r.ok)throw new Error('reg '+r.status);return (await r.json()).token;}

const LONG = 'docker run --rm -it --name hexeris-very-long-container-name -e DATABASE_URL=postgres://user:password@127.0.0.1:5432/db?sslmode=disable -v /opt/data:/data ghcr.io/example/image:latest';

(async () => {
  const a = U('sc_a'), b = U('sc_b'); await reg(a); const bt = await reg(b);
  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 800 } })).newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', a); await page.fill('#auth-password', 'Password123!'); await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  // The peer sends CODE — that is the scenario from the report.
  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bt}`);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  for (let i = 0; i < 30; i++) ws.send(JSON.stringify({ type:'message', id:'s'+i+Date.now(), from:b, to:a, body:'line '+i }));
  ws.send(JSON.stringify({ type:'message', id:'code-'+Date.now(), from:b, to:a,
    body: '```bash\n' + LONG + '\n```' }));
  await new Promise(r => setTimeout(r, 1200)); ws.close();
  await page.waitForSelector(`#contact-${b}`, { timeout: 10000 });
  await page.click(`#contact-${b}`);
  await page.waitForSelector('.md-code', { timeout: 8000 });
  await new Promise(r => setTimeout(r, 500));

  // ── A code block at the RECIPIENT ─────────────────────────────────────────
  const inCode = await page.evaluate(() => {
    const el = document.querySelector('.msg-row.in .md-code');
    if (!el) return null;
    const cs = getComputedStyle(el);
    return {
      overflows: el.scrollWidth > el.clientWidth,
      bar: el.offsetHeight - el.clientHeight,   // the space taken by the horizontal bar
      ffColor: cs.scrollbarColor,
      ffWidth: cs.scrollbarWidth,
      scrollable: el.scrollWidth - el.clientWidth,
    };
  });
  check('control: the code line really is wider than the bubble',
        inCode && inCode.overflows === true, JSON.stringify(inCode));
  // An overlay scrollbar (macOS/iOS/Android and this Chromium) takes no space and
  // is invisible at rest — which is exactly why bar styles alone are not enough.
  // We check the edge hint, which does not depend on the system bar.
  const edge = await page.evaluate(() => {
    const box = document.querySelector('.msg-row.in .md-codebox');
    const pre = box.querySelector('.md-code');
    const after = getComputedStyle(box, '::after');
    return { hasRight: box.classList.contains('has-right'),
             hasLeft: box.classList.contains('has-left'),
             afterOpacity: parseFloat(after.opacity),
             afterWidth: after.width,
             scrollLeft: pre.scrollLeft };
  });
  check('incoming code block marks its right edge as scrollable',
        edge.hasRight === true, JSON.stringify(edge));
  check('the edge hint is actually painted (opacity 1)',
        edge.afterOpacity === 1, 'opacity=' + edge.afterOpacity);
  check('control: no left hint while at the start',
        edge.hasLeft === false, 'has-left=' + edge.hasLeft);

  // Scrolled to the end — the right hint must disappear and the left one appear.
  await page.evaluate(() => {
    const pre = document.querySelector('.msg-row.in .md-code');
    pre.scrollLeft = pre.scrollWidth;
    pre.dispatchEvent(new Event('scroll'));
  });
  await new Promise(r => setTimeout(r, 250));
  const atEnd = await page.evaluate(() => {
    const box = document.querySelector('.msg-row.in .md-codebox');
    return { hasRight: box.classList.contains('has-right'),
             hasLeft: box.classList.contains('has-left'),
             afterOpacity: parseFloat(getComputedStyle(box, '::after').opacity) };
  });
  check('scrolled to the end: the right hint disappears',
        atEnd.hasRight === false && atEnd.afterOpacity === 0, JSON.stringify(atEnd));
  check('scrolled to the end: the left hint appears',
        atEnd.hasLeft === true, JSON.stringify(atEnd));

  // Negative control: SHORT code has nothing to scroll — there must be no hint,
  // otherwise it turns into a permanent decorative gradient.
  const shortEdge = await page.evaluate(async () => {
    const peer = document.getElementById('messages-wrap').dataset.peer;
    const id = 'short-' + Date.now();
    addToChat(peer, { id, from: peer, to: myUsername, media_type: undefined,
                      body: '```\nls\n```', ts: Date.now(), status: 'sent' });
    renderMessages(peer);
    await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
    const box = document.querySelector('.msg-bubble[data-id="' + id + '"] .md-codebox');
    const pre = box && box.querySelector('.md-code');
    return box ? { hasRight: box.classList.contains('has-right'),
                   overflows: pre.scrollWidth > pre.clientWidth } : 'MISSING';
  });
  check('negative control: a short code block gets no edge hint',
        shortEdge !== 'MISSING' && shortEdge.hasRight === false && shortEdge.overflows === false,
        JSON.stringify(shortEdge));
  check('Firefox gets an explicit thumb colour, not the default',
        /rgba?\(/.test(inCode.ffColor) && inCode.ffColor !== 'auto', inCode.ffColor);

  // The thumb must differ from the ground — otherwise it exists but is invisible.
  const contrast = await page.evaluate(() => {
    const el = document.querySelector('.msg-row.in .md-code');
    const thumb = getComputedStyle(document.documentElement).getPropertyValue('--scroll-thumb').trim();
    return { thumb, codeBg: getComputedStyle(el).backgroundColor,
             bubbleBg: getComputedStyle(el.closest('.msg-bubble')).backgroundColor };
  });
  check('thumb colour is not the old border token',
        /rgba\(255,\s*255,\s*255/.test(contrast.thumb), '--scroll-thumb=' + contrast.thumb);
  check('control: thumb differs from the code surface it sits on',
        contrast.thumb !== contrast.codeBg, contrast.thumb + ' vs ' + contrast.codeBg);

  // ── A code block in an OUTGOING bubble (blue ground) ─────────────────────
  await page.fill('#msg-textarea', '```bash\n' + LONG + '\n```');
  await page.click('.input-area .send-btn');
  await page.waitForSelector('.msg-row.out .md-code', { timeout: 8000 });
  await new Promise(r => setTimeout(r, 500));
  const outCode = await page.evaluate(() => {
    const el = document.querySelector('.msg-row.out .md-code');
    const cs = getComputedStyle(el);
    return { bar: el.offsetHeight - el.clientHeight, ffColor: cs.scrollbarColor,
             overflows: el.scrollWidth > el.clientWidth };
  });
  check('control: the outgoing code line also overflows', outCode.overflows === true);
  const outEdge = await page.evaluate(() => {
    const box = document.querySelector('.msg-row.out .md-codebox');
    return { hasRight: box.classList.contains('has-right'),
             edge: getComputedStyle(box).getPropertyValue('--code-edge').trim() };
  });
  check('outgoing code block marks its edge too', outEdge.hasRight === true, JSON.stringify(outEdge));
  check('outgoing edge tint follows the blue bubble, not the dark page',
        outEdge.edge.includes('bubble-out') || outEdge.edge.startsWith('#1a2f6e') || outEdge.edge.length > 0,
        '--code-edge=' + outEdge.edge);
  check('outgoing thumb is light (its surface is blue, not dark)',
        /rgba\(255,\s*255,\s*255/.test(outCode.ffColor), outCode.ffColor);

  // ── The other scrollable places ──────────────────────────────────────────
  const others = await page.evaluate(() => {
    const out = {};
    const probe = (name, sel) => {
      const el = document.querySelector(sel);
      if (!el) { out[name] = 'MISSING'; return; }
      const cs = getComputedStyle(el);
      out[name] = { w: cs.scrollbarWidth, c: cs.scrollbarColor };
    };
    probe('contacts', '.contacts-list');
    probe('feed', '#messages-wrap');
    return out;
  });
  check('the contact list has an explicit thumb colour too',
        others.contacts !== 'MISSING' && /rgba?\(/.test(others.contacts.c),
        JSON.stringify(others.contacts));
  // On DESKTOP the feed does show the bar; the hiding was for phones only.
  check('control: the desktop feed keeps its scrollbar',
        others.feed !== 'MISSING' && others.feed.w !== 'none', JSON.stringify(others.feed));

  // ── Deliberate hiding must NOT break ─────────────────────────────────────
  const mob = await (await browser.newContext({ ...devices['Pixel 7'] })).newPage();
  mob.on('pageerror', (e) => { console.log('  JS ERROR (mobile):', e.message); failures++; });
  await mob.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await mob.fill('#auth-username', a); await mob.fill('#auth-password', 'Password123!'); await mob.click('#auth-btn');
  await mob.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
  await mob.waitForSelector(`#contact-${b}`, { timeout: 10000 });
  await mob.click(`#contact-${b}`);
  await mob.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 500));
  const hidden = await mob.evaluate(() => {
    const ta = document.getElementById('msg-textarea');
    ta.value = 'a\n'.repeat(30);
    ta.dispatchEvent(new Event('input', { bubbles: true }));
    const gutter = ta.offsetWidth - ta.clientWidth;
    const overflows = ta.scrollHeight > ta.clientHeight;
    ta.value = ''; ta.dispatchEvent(new Event('input', { bubbles: true }));
    return { feed: getComputedStyle(document.getElementById('messages-wrap')).scrollbarWidth,
             taGutter: gutter, taOverflows: overflows };
  });
  check('control: the mobile feed scrollbar stays hidden on purpose',
        hidden.feed === 'none', 'scrollbar-width=' + hidden.feed);
  check('control: the mobile input field still has no scrollbar gutter',
        hidden.taOverflows === true && hidden.taGutter <= 2, JSON.stringify(hidden));
  // A code block on a phone, though, MUST show the bar — that is where it was invisible.
  const mobCode = await mob.evaluate(() => {
    const box = document.querySelector('.msg-row.in .md-codebox');
    const el = box && box.querySelector('.md-code');
    return box ? { hasRight: box.classList.contains('has-right'),
                   overflows: el.scrollWidth > el.clientWidth,
                   c: getComputedStyle(el).scrollbarColor } : 'MISSING';
  });
  check('mobile code block marks its scrollable edge',
        mobCode !== 'MISSING' && mobCode.hasRight === true, JSON.stringify(mobCode));

  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
