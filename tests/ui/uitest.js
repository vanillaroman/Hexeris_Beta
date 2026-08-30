// An end-to-end check of conversation preferences through a real server and a
// real UI.
//
// We check not "the function was called" but the observable consequences: did the
// row disappear from the list, did the push go out, is the notification shown.
// Every assertion has a negative control — otherwise a test that is always green
// proves nothing.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');

const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);

let failures = 0;
function check(name, ok, extra) {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + name + (extra ? '  — ' + extra : ''));
  if (!ok) failures++;
}

async function register(username) {
  const r = await fetch(BASE + '/register', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password: 'Password123!' })
  });
  if (!r.ok) throw new Error('register ' + username + ': ' + r.status + ' ' + await r.text());
  return (await r.json()).token;
}

async function login(page, username) {
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', username);
  await page.fill('#auth-password', 'Password123!');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
}

(async () => {
  const alice = U('ui_alice'), bob = U('ui_bob'), carol = U('ui_carol');
  await register(alice);
  const bobTok = await register(bob);
  const carolTok = await register(carol);

  // Bob and Carol write to Alice — two conversations appear in her list.
  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const ctx = await browser.newContext({ viewport: { width: 1200, height: 800 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  page.on('console', (m) => { if (m.type() === 'error') console.log('  console.error:', m.text()); });

  await login(page, alice);

  for (const [who, tok] of [[bob, bobTok], [carol, carolTok]]) {
    const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${tok}`);
    await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
    ws.send(JSON.stringify({ type: 'message', id: String(Date.now()) + who,
      from: who, to: alice, body: 'hello from ' + who }));
    await new Promise(r => setTimeout(r, 400));
    ws.close();
  }
  await page.waitForSelector(`#contact-${bob}`, { timeout: 8000 });
  await page.waitForSelector(`#contact-${carol}`, { timeout: 8000 });
  check('both conversations appeared in the list', true);

  // ── Context menu on right-click ──
  await page.click(`#contact-${bob}`, { button: 'right' });
  await page.waitForSelector('#chat-ctx-menu.visible', { timeout: 3000 });
  const labels = await page.$$eval('#chat-ctx-menu .ctx-item',
    els => els.filter(e => e.offsetParent !== null).map(e => e.textContent.trim()));
  check('the menu opened on right-click with the expected items', labels.length >= 4, labels.join(' | '));

  // ── Mute ──
  await page.click('#cctx-mute');
  await page.waitForSelector(`#contact-${bob}.muted`, { timeout: 4000 });
  // The class appears in the markup EARLIER than the write to the server
  // finishes: the interface does not wait for a response so it does not hang on a
  // click. A single read right after the class therefore sometimes caught the
  // server before the write — and the test flickered. We wait for the preference
  // to actually appear rather than guessing a pause.
  const auth = { Authorization: 'Bearer ' + await page.evaluate(() => token) };
  let prefs = {};
  for (let i = 0; i < 40; i++) {
    prefs = await (await fetch(BASE + '/chats/prefs', { headers: auth })).json();
    if (prefs[bob] && prefs[bob].muted) break;
    await new Promise(r => setTimeout(r, 100));
  }
  check('mute is saved on the server', !!(prefs[bob] && prefs[bob].muted), JSON.stringify(prefs[bob]));
  check('mute was applied to one conversation only', !(prefs[carol] && prefs[carol].muted));

  // A notification is not shown in a muted conversation, but is in an ordinary one.
  // The permission in headless is always 'denied', so we stub that too, otherwise
  // the control case returns false as well and the test checks nothing.
  const notif = await page.evaluate(({ mutedPeer, livePeer }) => {
    Object.defineProperty(Notification, 'permission', { get: () => 'granted', configurable: true });
    const seen = [];
    const Orig = window.Notification;
    window.Notification = function (title, opts) { seen.push(title); return { close() {}, onclick: null }; };
    window.Notification.permission = 'granted';
    Object.defineProperty(document, 'visibilityState', { get: () => 'hidden', configurable: true });
    showNotification(mutedPeer, 'text');
    showNotification(livePeer, 'text');
    window.Notification = Orig;
    return seen;
  }, { mutedPeer: bob, livePeer: carol });
  check('a muted conversation shows no notification', !notif.some(t => t.includes(bob)), JSON.stringify(notif));
  check('control: an unmuted one does show it', notif.some(t => t.includes(carol)), JSON.stringify(notif));

  // ── Archive ──
  await page.click(`#contact-${bob}`, { button: 'right' });
  await page.waitForSelector('#chat-ctx-menu.visible');
  await page.click('#cctx-archive');
  await page.waitForSelector(`#contact-${bob}`, { state: 'detached', timeout: 4000 });
  check('the archived conversation was removed from the list', true);
  const arcVisible = await page.isVisible('.archive-row');
  check('the "Archived" row appeared', arcVisible);
  check('the neighbouring conversation stayed', await page.isVisible(`#contact-${carol}`));

  // Expand the archive — the conversation is visible again.
  await page.click('.archive-row');
  await page.waitForSelector(`#contact-${bob}.archived`, { timeout: 4000 });
  check('the expanded archive shows the conversation', true);

  // Unarchive.
  await page.click(`#contact-${bob}`, { button: 'right' });
  await page.waitForSelector('#chat-ctx-menu.visible');
  await page.click('#cctx-archive');
  await page.waitForSelector(`#contact-${bob}:not(.archived)`, { timeout: 4000 });
  check('unarchiving returned the conversation to the main list', true);

  // ── Deleting a chat ──
  await page.click(`#contact-${carol}`, { button: 'right' });
  await page.waitForSelector('#chat-ctx-menu.visible');
  await page.click('#cctx-delete');
  await page.waitForSelector('#hex-modal-overlay.open', { timeout: 3000 });
  const confirmText = await page.textContent('#hex-modal-msg');
  check('the confirmation honestly mentions the peer\'s copy',
    /still have their copy/i.test(confirmText), confirmText);
  await page.click('#hex-modal-ok');
  await page.waitForSelector(`#contact-${carol}`, { state: 'detached', timeout: 5000 });
  check('the deleted conversation disappeared from the list', true);

  // Reload: the conversation must not come back from history.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
  await page.waitForTimeout(2500);
  check('after a reload the deleted conversation did not come back',
    !(await page.isVisible(`#contact-${carol}`)));
  check('control: the undeleted conversation is in place', await page.isVisible(`#contact-${bob}`));
  check('mute survived the reload', await page.isVisible(`#contact-${bob}.muted`));

  // The peer still has the conversation — "deleted for me", not "for everyone".
  const carolHist = await (await fetch(BASE + `/history?peer=${alice}&limit=50`, {
    headers: { Authorization: 'Bearer ' + carolTok }
  })).json();
  check('the peer still has the conversation', carolHist.length > 0, 'messages: ' + carolHist.length);

  await browser.close();
  console.log(failures ? `\nFAILED checks: ${failures}` : '\nAll checks passed');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('ERROR:', e); process.exit(2); });
