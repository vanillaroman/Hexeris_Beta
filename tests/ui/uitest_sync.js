// History sync: everything arrived, everything was saved, no errors swallowed.
//
// ═══ WHY THIS SUITE EXISTS ═══════════════════════════════════════════════
//
// A change went into loadHistory that made the end of the function reference an
// undeclared variable. The ReferenceError was thrown AFTER the messages had
// already been distributed into chats, and it landed in a catch that writes a
// warning to the console and carries on. On screen everything looked right.
//
// What did not run was the whole tail of the function: saving the messages to
// localStorage, the final sort and repaint, restoring the open conversation, the
// reaction top-up sync. The globalSince cursor, meanwhile, was saved INSIDE the
// loop — that is, after a reload the client believed it had already fetched
// everything while storage was empty. The history disappeared.
//
// The full UI test set missed this, and not by chance: the exception is eaten by
// the catch, it never reaches pageerror, and the contact list manages to render
// inside the loop. So what is checked here is not "there are rows on screen" but
// three things that break silently:
//
//   1. the console contains no sync errors;
//   2. the messages REACHED localStorage, not only memory;
//   3. they survive a reload.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : ''));
  if (!ok) failures++;
};
const tag = 'sy' + String(Date.now()).slice(-7);
const PEERS = 4, PER_PEER = 12;

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
    setTimeout(() => { s.close(); res(); }, 1200);
  };
  s.onerror = rej;
});

const snapshot = (page) => page.evaluate(() => ({
  rows: document.querySelectorAll('.contact-item').length,
  chats: Object.keys(typeof chats !== 'undefined' ? chats : {}).length,
  memory: Object.values(typeof chats !== 'undefined' ? chats : {}).reduce((a, v) => a + v.length, 0),
  storedKeys: Object.keys(localStorage).filter((k) => k.startsWith('hc_msgs_')).length,
  stored: Object.keys(localStorage).filter((k) => k.startsWith('hc_msgs_'))
    .reduce((a, k) => { try { return a + JSON.parse(localStorage.getItem(k) || '[]').length; } catch { return a; } }, 0),
  cursor: localStorage.getItem('hc_globalsince_' + myUsername),
}));

(async () => {
  await waitReady();
  const me = 'me' + tag;
  await reg(me);
  for (let i = 0; i < PEERS; i++) {
    const p = 'p' + i + tag;
    const tp = await reg(p);
    await seed(tp, Array.from({ length: PER_PEER }, (_, k) => ({
      type: 'message', to: me, body: `${p} message ${k}`,
    })));
  }
  const expected = PEERS * PER_PEER;

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
  const page = await ctx.newPage();

  // Sync errors live in console.warn rather than pageerror: they are caught by a
  // catch inside loadHistory. We catch them separately — those are what was missed.
  const syncLog = [];
  page.on('console', (m) => {
    const t = m.text();
    if (/loadHistory error|history sync:/.test(t)) syncLog.push(t.slice(0, 120));
  });
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#auth-screen', { state: 'visible' });
  await page.fill('#auth-username', me);
  await page.fill('#auth-password', 'passw0rd-test');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(5000);

  const first = await snapshot(page);
  check('all conversations are in the list', first.rows === PEERS, `rows ${first.rows}, expected ${PEERS}`);
  check('all messages are in memory', first.memory === expected,
        `${first.memory} of ${expected}`);
  // The main check. The tail of loadHistory (saving) runs AFTER rendering, so the
  // screen can be correct while storage is empty — and then the history disappears
  // on the next load.
  check('the messages are saved in localStorage', first.stored === expected,
        `${first.stored} of ${expected} in storage`);
  check('a key was created for every conversation', first.storedKeys === PEERS,
        `keys ${first.storedKeys}`);
  check('the sync cursor is saved', !!first.cursor && +first.cursor > 0, 'cursor=' + first.cursor);
  check('the sync reported no errors', syncLog.length === 0, syncLog.join(' | '));

  // Reload: the cursor is already saved, so the server will return no new
  // messages — the history must come up FROM STORAGE. If the tail of the function
  // did not run, the emptiness shows up here.
  syncLog.length = 0;
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(4000);

  const second = await snapshot(page);
  check('the conversations are in place after the reload', second.rows === PEERS, `rows ${second.rows}`);
  check('the messages are in place after the reload', second.memory === expected,
        `${second.memory} of ${expected}`);
  check('the reload neither added nor lost anything',
        second.memory === first.memory && second.rows === first.rows,
        JSON.stringify({ first, second }));
  check('the sync reported no errors after the reload either', syncLog.length === 0, syncLog.join(' | '));
  check('no JS errors', pageErrors.length === 0, pageErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
