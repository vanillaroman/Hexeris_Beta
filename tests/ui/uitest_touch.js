// A long press on a phone: it opens the same menu and does NOT open the chat.
// The second matters more than the first — if an ordinary click also fires after
// a long press, the user gets a menu on top of a conversation just opened.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');

const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);

let failures = 0;
function check(name, ok, extra) {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + name + (extra ? '  — ' + extra : ''));
  if (!ok) failures++;
}

async function register(username) {
  const r = await fetch(BASE + '/register', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password: 'Password123!' })
  });
  if (!r.ok) throw new Error('register: ' + r.status);
  return (await r.json()).token;
}

(async () => {
  const alice = U('tp_alice'), bob = U('tp_bob');
  await register(alice);
  const bobTok = await register(bob);

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const ctx = await browser.newContext({ ...devices['Pixel 7'] });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', alice);
  await page.fill('#auth-password', 'Password123!');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bobTok}`);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  ws.send(JSON.stringify({ type: 'message', id: String(Date.now()), from: bob, to: alice, body: 'hi' }));
  await new Promise(r => setTimeout(r, 500));
  ws.close();
  await page.waitForSelector(`#contact-${bob}`, { timeout: 8000 });

  const box = await page.locator(`#contact-${bob}`).boundingBox();
  const x = Math.round(box.x + box.width / 3), y = Math.round(box.y + box.height / 2);

  // A real sequence of touch events: the Playwright browser does not synthesise
  // them for a "long press" itself, so we send them by hand.
  const touchSeq = async (type, tx, ty) => {
    await page.evaluate(({ type, tx, ty }) => {
      const el = document.elementFromPoint(tx, ty);
      const t = new Touch({ identifier: 1, target: el, clientX: tx, clientY: ty });
      el.dispatchEvent(new TouchEvent(type, {
        bubbles: true, cancelable: true,
        touches: type === 'touchend' ? [] : [t],
        targetTouches: type === 'touchend' ? [] : [t],
        changedTouches: [t]
      }));
    }, { type, tx, ty });
  };

  await touchSeq('touchstart', x, y);
  await page.waitForTimeout(650);           // longer than the 500 ms threshold
  const menuOpen = await page.isVisible('#chat-ctx-menu.visible');
  check('the long press opened the menu', menuOpen);

  await touchSeq('touchend', x, y);
  // A real finger produces a click after a long press — we check that the chat
  // does not open from it.
  await page.mouse.click(x, y);
  await page.waitForTimeout(400);
  const chatOpened = await page.isVisible('#chat-main');
  check('the chat did not open after the long press', !chatOpened);

  // The menu works: mute the conversation from here.
  await page.click('#cctx-mute');
  await page.waitForSelector(`#contact-${bob}.muted`, { timeout: 4000 });
  check('the menu item fired on tap', true);

  // Control: a short tap on the row opens the chat as usual.
  await page.waitForTimeout(300);
  await touchSeq('touchstart', x, y);
  await touchSeq('touchend', x, y);
  await page.mouse.click(x, y);
  await page.waitForTimeout(500);
  check('control: a short tap opens the chat', await page.isVisible('#chat-main'));

  // A scroll during a long press must not turn into a menu.
  await page.click('.back-btn').catch(() => {});
  await page.waitForTimeout(300);

  await browser.close();
  console.log(failures ? `\nFAILED checks: ${failures}` : '\nAll checks passed');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('ERROR:', e); process.exit(2); });
