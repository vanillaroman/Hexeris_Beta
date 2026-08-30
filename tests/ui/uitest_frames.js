// Smoothness: long frames while scrolling a long history.
//
// Why a separate suite. Contrast and geometry are measured statically — you look
// at a finished page and count. A stutter cannot be caught that way: it lives in
// time and is visible only as a frame that took longer to draw than the rest.
// Before this suite the project had nothing that measured such a thing, and the
// phrase "it lags" stayed unverifiable.
//
// What is measured. Through requestAnimationFrame we collect the intervals
// between frames during a programmatic scroll. The averages are not interesting —
// they are always pretty — but the tail of the distribution is: p95 and the number
// of frames longer than 50 ms. One such frame is noticed as a hitch, a series of
// them as "it is lagging".
//
// The thresholds are deliberately lenient. The suite runs in headless Chromium on
// a shared machine where a neighbouring process can easily steal a frame or two;
// the goal is not to certify 60 fps but to catch a REGRESSION where, after a
// change, the list starts repainting in full on every frame. A threshold that
// fails every other run stops being read, and then it catches nothing.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : ''));
  if (!ok) failures++;
};
const tag = 'fr' + String(Date.now()).slice(-7);

// How many messages we seed. 400 is already a long conversation while keeping the
// suite inside a minute.
const SEED_COUNT = +(process.env.FRAMES_SEED || 400);
const LONG_FRAME_MS = 50;

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

// Seeding goes through a single socket in one batch: one message per connection
// would make these 400 take minutes.
const seed = (token, msgs) => new Promise((res, rej) => {
  const s = new WebSocket(BASE.replace('http', 'ws') + '/ws?token=' + token);
  s.onopen = () => {
    msgs.forEach((m, i) => s.send(JSON.stringify({ ...m, id: tag + '-' + i })));
    setTimeout(() => { s.close(); res(); }, 1200 + msgs.length * 2);
  };
  s.onerror = rej;
});

// Frame collection lives in the page: inter-frame intervals are invisible from outside.
const COLLECT = `(() => new Promise((resolve) => {
  const el = document.getElementById('messages-wrap');
  if (!el) { resolve(null); return; }
  const gaps = [];
  let prev = performance.now();
  let frames = 0;
  const total = el.scrollHeight - el.clientHeight;
  el.scrollTop = 0;
  const step = Math.max(8, Math.round(total / 90));
  function tick(now) {
    gaps.push(now - prev);
    prev = now;
    frames++;
    el.scrollTop = Math.min(total, el.scrollTop + step);
    if (frames < 90 && el.scrollTop < total) requestAnimationFrame(tick);
    else {
      // The first interval is discarded: it measures the pause before the scroll
      // starts rather than the scroll itself.
      const g = gaps.slice(1).sort((a, b) => a - b);
      const at = (q) => g[Math.min(g.length - 1, Math.floor(g.length * q))] || 0;
      resolve({
        frames: g.length,
        median: +at(0.5).toFixed(1),
        p95: +at(0.95).toFixed(1),
        worst: +(g[g.length - 1] || 0).toFixed(1),
        long: g.filter((x) => x > ${LONG_FRAME_MS}).length,
        scrolled: Math.round(el.scrollTop),
        total: Math.round(total),
      });
    }
  }
  requestAnimationFrame(tick);
}))()`;

(async () => {
  await waitReady();
  const me = 'me' + tag, bob = 'bob' + tag;
  const tMe = await reg(me); const tBob = await reg(bob);

  // Half the messages in one direction, half in the other: a one-sided feed is
  // cheaper to draw than a real conversation, and the result would be better than
  // the truth.
  const half = Math.floor(SEED_COUNT / 2);
  const mk = (to, n, who) => Array.from({ length: n }, (_, i) => ({
    type: 'message', to,
    body: who + ' #' + i + ' — ' +
          'a line of realistic length so wrapping and layout cost are measured, not a stub.',
  }));
  await seed(tBob, mk(me, half, 'from bob'));
  await seed(tMe, mk(bob, SEED_COUNT - half, 'from me'));

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  const jsErrors = [];

  for (const dev of [
    { name: 'desktop', viewport: { width: 1280, height: 800 } },
    { name: 'phone', viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
  ]) {
    const { name, ...opts } = dev;
    const ctx = await browser.newContext({ ...opts, serviceWorkers: 'block' });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push(name + ': ' + e.message));

    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#auth-screen', { state: 'visible' });
    await page.fill('#auth-username', me);
    await page.fill('#auth-password', 'passw0rd-test');
    await page.click('#auth-btn');
    await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
    await page.waitForTimeout(1500);

    const row = page.locator('.contact-item').first();
    if (await row.count()) { await row.click(); }
    await page.waitForTimeout(2500);

    // Control: the history really is long. Without it the suite would "pass" on
    // an empty chat, having measured nothing.
    const bubbles = await page.evaluate(() => document.querySelectorAll('.msg-bubble').length);
    check(`${name}: enough history collected to measure (control)`, bubbles >= 100,
          'bubbles ' + bubbles);
    if (bubbles < 100) { await ctx.close(); continue; }

    const m = await page.evaluate(COLLECT);
    if (!m) { check(`${name}: the measurement happened`, false, 'no #messages-wrap'); await ctx.close(); continue; }

    console.log(`  ${name}: frames ${m.frames}, median ${m.median}ms, ` +
                `p95 ${m.p95}ms, worst ${m.worst}ms, longer than ${LONG_FRAME_MS}ms — ${m.long}`);

    check(`${name}: the scroll really happened (control)`, m.scrolled > 0,
          `travelled ${m.scrolled} of ${m.total}`);
    // A p95 of 100ms means "ten frames per second in the worst case". Anything
    // worse and a person no longer reads it as scrolling.
    check(`${name}: frame p95 within 100ms`, m.p95 <= 100, m.p95 + 'ms');
    // Individual long frames are unavoidable (the first render, garbage
    // collection). A quarter of the frames is no longer an outlier but a property.
    check(`${name}: fewer than a quarter of the frames are long`, m.long <= m.frames * 0.25,
          `${m.long} of ${m.frames}`);

    await ctx.close();
  }

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
