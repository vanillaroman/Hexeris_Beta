// Three fixes: the order in the archive, no text selection when the menu opens,
// and closing the menu with a swipe on a phone.
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

async function say(tok, from, to, body) {
  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${tok}`);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  ws.send(JSON.stringify({ type: 'message', id: String(Date.now()) + Math.random(), from, to, body }));
  await new Promise(r => setTimeout(r, 350));
  ws.close();
}

async function login(page, username) {
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', username);
  await page.fill('#auth-password', 'Password123!');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
}

// The order of the sidebar rows as the user sees it.
async function listOrder(page) {
  return page.$$eval('#contacts-list .contact-item', els => els.map(e => e.dataset.peer));
}

async function openMenu(page, peer) {
  await page.click(`#contact-${peer}`, { button: 'right' });
  await page.waitForSelector('#chat-ctx-menu.visible', { timeout: 3000 });
}
async function archive(page, peer) {
  await openMenu(page, peer);
  await page.click('#cctx-archive');
  await page.waitForTimeout(500);
}

(async () => {
  const me = U('pl_me');
  const peers = [U('pl_p1'), U('pl_p2'), U('pl_p3')];
  await register(me);
  const toks = [];
  for (const p of peers) toks.push(await register(p));

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });

  // ── 1. Order in the archive ──
  const ctx = await browser.newContext({ viewport: { width: 1200, height: 800 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  await login(page, me);
  for (let i = 0; i < peers.length; i++) await say(toks[i], peers[i], me, 'hello ' + i);
  for (const p of peers) await page.waitForSelector(`#contact-${p}`, { timeout: 8000 });

  // We archive in the order p2 → p1 → p3 (deliberately NOT the message order).
  const archSeq = [peers[1], peers[0], peers[2]];
  for (const p of archSeq) { await archive(page, p); await page.waitForTimeout(1100); }
  // The expected order IN THE LIST: the last one put away on top.
  const archOrder = [...archSeq].reverse();

  await page.click('.archive-row');
  await page.waitForTimeout(400);
  let order = await listOrder(page);
  check('in the archive the last one put away is on top, not the message order',
    JSON.stringify(order) === JSON.stringify(archOrder), order.join(' → '));

  // A new message in an archived chat must not reshuffle the section — that is
  // exactly what made the order "float".
  await say(toks[2], peers[2], me, 'a fresh message to the last archived one');
  await page.waitForTimeout(900);
  order = await listOrder(page);
  check('a fresh message did not move the archived chat to the top',
    JSON.stringify(order) === JSON.stringify(archOrder), order.join(' → '));

  // Unarchive and archive again — it goes to the top, not back to its old place.
  // We take the BOTTOM one in the list: the top one would end up on top without any
  // new stamp, and the check would mean nothing.
  const bottom = archOrder[archOrder.length - 1];
  await archive(page, bottom);                  // out of the archive
  await page.waitForTimeout(300);
  await archive(page, bottom);                  // back into the archive
  await page.waitForTimeout(600);
  order = (await listOrder(page)).filter(p => peers.includes(p));
  check('a re-archived chat rose from the bottom to the top',
    order[0] === bottom, order.join(' → ') + ' (re-archived ' + bottom + ')');

  // The real archiving time comes from the server, not only from localStorage.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
  await page.waitForTimeout(2500);
  await page.click('.archive-row');
  await page.waitForTimeout(400);
  const afterReload = (await listOrder(page)).filter(p => peers.includes(p));
  check('the order survived a reload (the stamp from the server)',
    JSON.stringify(afterReload) === JSON.stringify(order), afterReload.join(' → '));

  // ── 2. Text selection ──
  // The message is long so there is something to select.
  await say(toks[0], peers[0], me, 'A long message that can be selected with the mouse in full');
  await page.waitForTimeout(700);
  await page.click(`#contact-${peers[0]}`);
  await page.waitForSelector('.msg-bubble', { timeout: 5000 });

  // Control: without the menu the message body is still selectable — copying
  // messages must not be broken.
  const canSelectNormally = await page.evaluate(() => {
    const b = document.querySelector('.msg-bubble');
    return getComputedStyle(b).userSelect;
  });
  check('control: the message body is selectable while the menu is closed',
    canSelectNormally === 'text', canSelectNormally);

  // We imitate what the browser does on a long press: select text, then open the
  // menu with the right button.
  await page.evaluate(() => {
    const b = document.querySelector('.msg-bubble');
    const r = document.createRange();
    r.selectNodeContents(b);
    const s = window.getSelection();
    s.removeAllRanges(); s.addRange(r);
  });
  const selBefore = await page.evaluate(() => window.getSelection().toString().length);
  await page.click('.msg-bubble', { button: 'right' });
  await page.waitForSelector('#ctx-menu.visible', { timeout: 3000 });
  const selAfter = await page.evaluate(() => window.getSelection().toString().length);
  check('the selection is dropped when the message menu opens',
    selBefore > 0 && selAfter === 0, `was ${selBefore} characters, became ${selAfter}`);
  const bubbleSel = await page.evaluate(() =>
    getComputedStyle(document.querySelector('.msg-bubble')).userSelect);
  check('with the menu open the message body is not selectable', bubbleSel === 'none', bubbleSel);

  const itemSel = await page.evaluate(() =>
    getComputedStyle(document.querySelector('#ctx-menu .ctx-item')).userSelect);
  check('the menu item labels are not selectable', itemSel === 'none', itemSel);

  // The input field must not suffer: it may hold a draft with a caret.
  const taSel = await page.evaluate(() =>
    getComputedStyle(document.getElementById('msg-textarea')).userSelect);
  check('the input field stays selectable', taSel === 'text', taSel);

  await page.keyboard.press('Escape').catch(() => {});
  await page.mouse.click(600, 60);
  await page.waitForTimeout(300);
  const restored = await page.evaluate(() =>
    getComputedStyle(document.querySelector('.msg-bubble')).userSelect);
  check('selection is allowed again after the menu closes', restored === 'text', restored);

  await ctx.close();

  // ── 3. Closing the menu with a swipe on a phone ──
  const mctx = await browser.newContext({ ...devices['Pixel 7'] });
  const mpage = await mctx.newPage();
  mpage.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  // A fresh peer: all the previous ones are in the archive by now, and the archive
  // is collapsed in a new session — there would be nothing to long-press.
  const live = U('pl_live');
  const liveTok = await register(live);
  await say(liveTok, live, me, 'not in the archive');
  await login(mpage, me);
  await mpage.waitForSelector(`#contact-${live}`, { timeout: 8000 });

  const touch = (type, tx, ty) => mpage.evaluate(({ type, tx, ty }) => {
    const el = document.elementFromPoint(tx, ty) || document.body;
    const t = new Touch({ identifier: 1, target: el, clientX: tx, clientY: ty });
    el.dispatchEvent(new TouchEvent(type, {
      bubbles: true, cancelable: true,
      touches: type === 'touchend' ? [] : [t],
      targetTouches: type === 'touchend' ? [] : [t],
      changedTouches: [t]
    }));
  }, { type, tx, ty });

  const box = await mpage.locator(`#contact-${live}`).boundingBox();
  const px = Math.round(box.x + box.width / 3), py = Math.round(box.y + box.height / 2);

  // Open with a long press.
  await touch('touchstart', px, py);
  await mpage.waitForTimeout(650);
  await touch('touchend', px, py);
  check('the long press opened the menu', await mpage.isVisible('#chat-ctx-menu.visible'));

  // Right after opening, the menu must not collapse from its own gesture.
  await mpage.waitForTimeout(200);
  check('the menu did not close from the gesture that opened it',
    await mpage.isVisible('#chat-ctx-menu.visible'));

  // A swipe over an arbitrary area — the menu closes.
  await mpage.waitForTimeout(400);
  await touch('touchstart', 200, 500);
  await mpage.waitForTimeout(50);
  check('a touch outside the menu closes it',
    !(await mpage.isVisible('#chat-ctx-menu.visible')));

  // A swipe started OVER the menu closes it too.
  await touch('touchend', 200, 500);
  await mpage.waitForTimeout(400);
  await touch('touchstart', px, py);
  await mpage.waitForTimeout(650);
  await touch('touchend', px, py);
  await mpage.waitForTimeout(450);
  const mbox = await mpage.locator('#chat-ctx-menu').boundingBox();
  const mx = Math.round(mbox.x + mbox.width / 2), my = Math.round(mbox.y + 20);
  await touch('touchstart', mx, my);
  await touch('touchmove', mx, my + 60);
  await mpage.waitForTimeout(80);
  check('a swipe over the menu closes it',
    !(await mpage.isVisible('#chat-ctx-menu.visible')));

  // Control: a small finger tremor during an ordinary tap on an item does NOT
  // close the menu — otherwise the items would become unpressable.
  await touch('touchend', mx, my + 60);
  await mpage.waitForTimeout(400);
  await touch('touchstart', px, py);
  await mpage.waitForTimeout(650);
  await touch('touchend', px, py);
  await mpage.waitForTimeout(450);
  const mbox2 = await mpage.locator('#chat-ctx-menu').boundingBox();
  const ix = Math.round(mbox2.x + mbox2.width / 2), iy = Math.round(mbox2.y + 20);
  await touch('touchstart', ix, iy);
  await touch('touchmove', ix + 3, iy + 2);     // a 3 px tremor
  await mpage.waitForTimeout(80);
  check('control: a finger tremor on an item does not close the menu',
    await mpage.isVisible('#chat-ctx-menu.visible'));

  await browser.close();
  console.log(failures ? `\nFAILED checks: ${failures}` : '\nAll checks passed');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('ERROR:', e); process.exit(2); });
