// Cold start: a clean browser, a large history, many contacts.
//
// The symptom that was fixed: the contact list arrived in "waves" after every
// reload. The cause — the sync broke off on a page counter
// (50 × 200 = 10,000 messages) while the cursor was saved anyway, and the
// remainder was only loaded on the next run.
//
// We check exactly that: after ONE sign-in on a clean profile every contact and
// the whole history must be there, and a reload must add nothing.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
async function reg(u) {
  const r = await fetch(BASE + '/register', { method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: u, password: 'Password123!' }) });
  if (!r.ok) throw new Error('reg ' + r.status);
  return (await r.json()).token;
}

// Enough contacts and messages for the volume to exceed the former ceiling of
// 10,000 messages in one run beyond any doubt.
const PEERS = 12;
const PER_PEER = 950;

(async () => {
  const me = U('cs_me');
  await reg(me);

  const peers = [];
  for (let i = 0; i < PEERS; i++) {
    const name = U('cs_p' + i);
    peers.push({ name, token: await reg(name) });
  }

  console.log(`  filling: ${PEERS} contacts × ${PER_PEER} messages = ${PEERS * PER_PEER}`);
  const wsUrl = BASE.replace(/^http/, 'ws');
  for (const p of peers) {
    const ws = new WebSocket(`${wsUrl}/ws?token=${p.token}`);
    await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
    for (let i = 0; i < PER_PEER; i++) {
      ws.send(JSON.stringify({ type: 'message', id: p.name + '-' + i + '-' + Date.now(),
        from: p.name, to: me, body: 'message ' + i + ' from ' + p.name }));
    }
    await sleep(700);
    ws.close();
  }
  await sleep(3000);

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  // A clean context = "site data cleared".
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', me);
  await page.fill('#auth-password', 'Password123!');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });

  // Wait until the sync settles: the contact count stops growing.
  let stable = 0, last = -1, waited = 0;
  while (stable < 4 && waited < 90000) {
    await sleep(1000); waited += 1000;
    const n = await page.evaluate(() => document.querySelectorAll('.contact-item').length);
    if (n === last) stable++; else { stable = 0; last = n; }
  }

  const first = await page.evaluate(() => ({
    contacts: document.querySelectorAll('.contact-item').length,
    messages: Object.values(chats).reduce((s, a) => s + a.length, 0),
  }));
  console.log(`  after the FIRST sign-in: contacts ${first.contacts}, messages ${first.messages} (loading ~${waited / 1000}s)`);

  check('all contacts are visible after one sign-in, without reloads',
        first.contacts === PEERS, first.contacts + ' of ' + PEERS);
  check('the whole history is loaded in one run',
        first.messages >= PEERS * PER_PEER, first.messages + ' of ' + PEERS * PER_PEER);
  check('control: the volume is well above the former 10,000 ceiling',
        PEERS * PER_PEER > 10000, PEERS * PER_PEER + ' messages');

  // A reload must not ADD data — if it does, the first pass was incomplete, and
  // that is exactly the original symptom.
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });
  stable = 0; last = -1; waited = 0;
  while (stable < 4 && waited < 60000) {
    await sleep(1000); waited += 1000;
    const n = await page.evaluate(() => document.querySelectorAll('.contact-item').length);
    if (n === last) stable++; else { stable = 0; last = n; }
  }
  const second = await page.evaluate(() => ({
    contacts: document.querySelectorAll('.contact-item').length,
    messages: Object.values(chats).reduce((s, a) => s + a.length, 0),
  }));
  console.log(`  after the RELOAD: contacts ${second.contacts}, messages ${second.messages}`);

  check('a reload does NOT add contacts (so the first pass was complete)',
        second.contacts === first.contacts, first.contacts + ' → ' + second.contacts);
  check('a reload does NOT add messages',
        second.messages <= first.messages, first.messages + ' → ' + second.messages);

  // Contacts must appear gradually rather than in one lump at the very end:
  // on a cold start an empty sidebar for seconds reads as a breakage.
  const ctx2 = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page2 = await ctx2.newPage();
  await page2.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page2.fill('#auth-username', me);
  await page2.fill('#auth-password', 'Password123!');
  await page2.click('#auth-btn');
  await page2.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });
  let firstSeenAt = null;
  const t0 = Date.now();
  while (Date.now() - t0 < 30000) {
    const n = await page2.evaluate(() => document.querySelectorAll('.contact-item').length);
    if (n > 0) { firstSeenAt = Date.now() - t0; break; }
    await sleep(200);
  }
  check('the first contacts appear within the first seconds, not at the end of the sync',
        firstSeenAt !== null && firstSeenAt < 15000,
        firstSeenAt === null ? 'did not appear within 30s' : 'after ' + firstSeenAt + 'ms');

  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
