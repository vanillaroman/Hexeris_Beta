// Deleted accounts: do not ask about them in circles — and do not forget them forever.
//
// ═══ WHAT WAS WRONG ═══════════════════════════════════════════════════════
//
// A conversation with a deleted employee stays in the list — rightly so, the
// history has not gone anywhere. But on every open of such a conversation the
// client asked for the person's presence (/status) and profile (/api/profile),
// and both answered 404. In the console that looked like a breakage even though
// nothing is broken; on the server, like an extra pair of requests for every such
// contact on every sign-in.
//
// ═══ WHAT MUST NOT HAPPEN WHILE FIXING IT ════════════════════════════════
//
// An eternal "deleted" mark is a cure worse than the disease: a username can be
// registered again, and the new person would be left without a name, avatar and
// presence — silently at that. So the memory has an expiry (12 hours) AND the mark
// is cleared immediately on any sign of life — an incoming message, a profile
// broadcast, an explicit username check by hand.
//
// The suite checks both halves: that there are no extra requests, and that a
// returned person is visible at once rather than half a day later.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : ''));
  if (!ok) failures++;
};
const tag = 'gn' + String(Date.now()).slice(-7);

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

const seed = (token, msgs) => new Promise((res, rej) => {
  const s = new WebSocket(BASE.replace('http', 'ws') + '/ws?token=' + token);
  s.onopen = () => {
    msgs.forEach((m, i) => s.send(JSON.stringify({ ...m, id: tag + '-' + Math.random().toString(36).slice(2) + i })));
    setTimeout(() => { s.close(); res(); }, 1500);
  };
  s.onerror = rej;
});

(async () => {
  await waitReady();
  const me = 'me' + tag;
  const ghost = 'ghost' + tag;   // at first it does not exist at all, later we create it
  await reg(me);

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
  const page = await ctx.newPage();
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));

  // We count exactly the two requests that made the console noisy.
  let statusAsks = 0, profileAsks = 0;
  page.on('request', (r) => {
    const u = r.url();
    if (!u.includes('user=' + ghost)) return;
    if (u.includes('/status?')) statusAsks++;
    else if (u.includes('/api/profile?')) profileAsks++;
  });

  const login = async () => {
    await page.waitForSelector('#auth-screen', { state: 'visible' });
    await page.fill('#auth-username', me);
    await page.fill('#auth-password', 'passw0rd-test');
    await page.click('#auth-btn');
    await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  };

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await login();

  // A conversation with a non-existent username — exactly what is left after an
  // employee is deleted: the messages are in the cache, the account is not on the server.
  await page.evaluate(([me, ghost]) => {
    const now = Date.now();
    localStorage.setItem('hc_msgs_' + me + '_' + ghost, JSON.stringify([
      { id: 'g1', seq: 0, from: ghost, to: me, body: 'left the company', status: 'delivered', ts: now, created_at: now },
    ]));
  }, [me, ghost]);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(2500);

  const row = await page.$('#contact-' + ghost);
  check('the conversation with the deleted peer stayed in the list (control)', !!row,
        row ? '' : 'there is no #contact-' + ghost + ' row — nothing to measure');
  if (!row) { await browser.close(); process.exit(1); }

  statusAsks = 0; profileAsks = 0;
  await page.click('#contact-' + ghost);
  await page.waitForTimeout(1500);
  check('first open: the person is asked about', statusAsks >= 1 && profileAsks >= 1,
        `status=${statusAsks}, profile=${profileAsks}`);

  // We switch back and forth: within one page load there must be no repeats —
  // the answer is already known.
  const asked1 = { s: statusAsks, p: profileAsks };
  await page.evaluate(() => { if (typeof showSidebar === 'function') showSidebar(); });
  await page.click('#contact-' + ghost);
  await page.waitForTimeout(1200);
  check('a repeat open: silently', statusAsks === asked1.s && profileAsks === asked1.p,
        `status ${asked1.s}→${statusAsks}, profile ${asked1.p}→${profileAsks}`);

  // Reload. The mark must survive it — otherwise every sign-in repeats the same
  // pair of 404s for every deleted contact.
  statusAsks = 0; profileAsks = 0;
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(2000);
  await page.click('#contact-' + ghost);
  await page.waitForTimeout(1500);
  check('after a reload there is no re-asking', statusAsks === 0 && profileAsks === 0,
        `status=${statusAsks}, profile=${profileAsks}`);

  const marked = await page.evaluate((g) => {
    try { return !!JSON.parse(localStorage.getItem('hc_gone_users') || '{}')[g]; } catch (e) { return false; }
  }, ghost);
  check('the mark is persisted rather than living only in the tab memory', marked);

  // ── The second half: the username was registered again ──────────────────
  //
  // That is the cost of getting it wrong if the mark were eternal: a new person
  // with the same username would stay invisible. An incoming message must clear it
  // at once.
  const tGhost = await reg(ghost);
  await seed(tGhost, [{ type: 'message', to: me, body: 'new hire, same handle' }]);
  await page.waitForTimeout(2000);

  const cleared = await page.evaluate((g) => {
    try { return !JSON.parse(localStorage.getItem('hc_gone_users') || '{}')[g]; } catch (e) { return false; }
  }, ghost);
  check('an incoming message clears the mark immediately', cleared,
        cleared ? '' : 'the username stayed marked deleted even though the person is writing');

  statusAsks = 0; profileAsks = 0;
  await page.evaluate(() => { if (typeof showSidebar === 'function') showSidebar(); });
  await page.click('#contact-' + ghost);
  await page.waitForTimeout(1500);
  check('a returned peer is asked about again', statusAsks >= 1 && profileAsks >= 1,
        `status=${statusAsks}, profile=${profileAsks}`);

  // ── The third half: a row with a deleted peer can be removed ────────────
  //
  // The server answered /chats/clear with a 404 if the peer was not in users. It
  // came out backwards: the conversation the person DEFINITELY wants gone was the
  // only undeletable one, and the client blamed the connection for it.
  const ghost2 = 'gone2' + tag;
  await page.evaluate(([me, g]) => {
    const now = Date.now();
    localStorage.setItem('hc_msgs_' + me + '_' + g, JSON.stringify([
      { id: 'g2', seq: 0, from: g, to: me, body: 'former colleague', status: 'delivered', ts: now, created_at: now },
    ]));
  }, [me, ghost2]);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(2500);
  check('the second phantom row is in place (control)', !!(await page.$('#contact-' + ghost2)),
        (await page.$('#contact-' + ghost2)) ? '' : 'the row is missing — nothing to delete');

  const toasts = [];
  await page.exposeFunction('_uitestToast', (t) => toasts.push(t));
  await page.evaluate(() => {
    const orig = window.toast;
    window.toast = (msg, kind) => { window._uitestToast(String(msg)); return orig && orig(msg, kind); };
  });

  // IMPORTANT: we do NOT await the evaluate. deleteChat is blocked on its own
  // confirmation window, and the button can only be pressed from here — awaiting
  // here would mean a permanent mutual deadlock (the first version of the suite
  // hung exactly like that).
  const delDone = page.evaluate((g) => deleteChat(g), ghost2);
  // hexConfirm is our own window, not the browser's: we confirm with the button.
  await page.waitForSelector('#hex-modal-overlay.open', { timeout: 5000 });
  await page.click('#hex-modal-ok');
  await delDone;
  await page.waitForTimeout(1000);

  const stillThere = !!(await page.$('#contact-' + ghost2));
  check('a conversation with a deleted peer can be deleted', !stillThere,
        stillThere ? 'the row stayed in place' : '');
  check('the connection is not blamed', !toasts.some((t) => /connection/i.test(t)),
        toasts.join(' | '));
  const gone2Stored = await page.evaluate((k) => localStorage.getItem(k) === null,
                                          'hc_msgs_' + me + '_' + ghost2);
  check('the deleted peer\'s message cache was wiped', gone2Stored);

  // ── Cleanup in bulk ─────────────────────────────────────────────────────
  const ghosts = ['bulkA' + tag, 'bulkB' + tag, 'bulkC' + tag];
  await page.evaluate(([me, list]) => {
    const now = Date.now();
    for (const g of list) {
      localStorage.setItem('hc_msgs_' + me + '_' + g, JSON.stringify([
        { id: 'b' + g, seq: 0, from: g, to: me, body: 'old', status: 'delivered', ts: now, created_at: now },
      ]));
    }
  }, [me, ghosts]);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(2500);

  const rowsBefore = await page.evaluate(() => document.querySelectorAll('.contact-item').length);
  const sweepDone = page.evaluate(() => cleanupDeletedChats());   // also without await
  await page.waitForSelector('#hex-modal-overlay.open', { timeout: 20000 });
  // The question must name them: nobody is asked to agree blindly.
  const askText = await page.evaluate(() => document.getElementById('hex-modal-msg').textContent);
  const named = ghosts.every((g) => askText.includes(g));
  check('the cleanup names the accounts individually', named, named ? '' : askText.slice(0, 160));
  await page.click('#hex-modal-ok');
  await sweepDone;
  await page.waitForTimeout(1000);

  const rowsAfter = await page.evaluate(() => document.querySelectorAll('.contact-item').length);
  check('the cleanup removed every phantom row', rowsBefore - rowsAfter === ghosts.length,
        `was ${rowsBefore}, now ${rowsAfter}, expected minus ${ghosts.length}`);
  // A live peer must survive — otherwise the cleanup wipes a conversation.
  const aliveKept = !!(await page.$('#contact-' + ghost));
  check('the cleanup left the live conversation alone', aliveKept,
        aliveKept ? '' : 'the live peer row disappeared');

  check('no JS errors', pageErrors.length === 0, pageErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
