// Service worker: what gets into Cache Storage and what never does.
//
// This check appeared after a finding: the handler intercepted ANY same-origin
// GET, and responses from /history, /files/ and /api/profile settled on the
// browser's disk. A decrypted conversation in the cache is data that neither
// signing out nor revoking a token erases.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };

async function reg(u) {
  const r = await fetch(BASE + '/register', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: u, password: 'Password123!' }),
  });
  if (r.status === 429) throw new Error('registration rate-limited (REGISTER_MAX_PER_IP) — raise it on the instance');
  if (!r.ok) throw new Error('reg ' + r.status);
  return (await r.json()).token;
}

async function waitReady() {
  for (let i = 0; i < 40; i++) {
    try { if ((await fetch(BASE + '/')).ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error('instance not ready at ' + BASE);
}

(async () => {
  await waitReady();
  const a = U('sw_a'), b = U('sw_b');
  const ta = await reg(a); await reg(b);

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  // The service worker IS needed here — it is the subject of the check.
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  const jsErrors = [];
  page.on('pageerror', (e) => jsErrors.push(e.message));

  await page.addInitScript((t) => { localStorage.setItem('hc_token', t); }, ta);
  await page.evaluate(() => {}).catch(() => {});
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });

  // Wait until the worker takes control: before that it intercepts nothing.
  const controlled = await page.waitForFunction(
    () => navigator.serviceWorker && navigator.serviceWorker.controller !== null,
    null, { timeout: 20000 }).then(() => true).catch(() => false);
  check('the service worker controls the page', controlled);
  if (!controlled) { await browser.close(); process.exit(1); }

  // We hit the private endpoints FROM the page — that is, through the worker.
  await page.evaluate(async (token) => {
    const h = { 'Authorization': 'Bearer ' + token };
    await fetch('/history?since=0&limit=5', { headers: h }).catch(() => {});
    await fetch('/api/profile', { headers: h }).catch(() => {});
    await fetch('/search?q=test', { headers: h }).catch(() => {});
  }, ta);
  await page.waitForTimeout(1200);

  const cached = await page.evaluate(async () => {
    const names = await caches.keys();
    const out = [];
    for (const n of names) {
      const keys = await (await caches.open(n)).keys();
      for (const k of keys) out.push(new URL(k.url).pathname + new URL(k.url).search);
    }
    return out;
  });

  const leaked = cached.filter((u) => /^\/(history|search|api\/|files\/|admin\/|groups|chats\/|unfurl|reactions)/.test(u));
  check('private responses did NOT reach Cache Storage', leaked.length === 0, leaked.join(', '));

  // Control: the shell IS cached — otherwise the check above would pass simply
  // because the cache is empty, and offline would not work.
  const shell = cached.filter((u) => u.startsWith('/js/') || u.startsWith('/css/') || u === '/index.html' || u === '/');
  check('the shell is cached (control: the cache is not empty)', shell.length >= 5, 'entries: ' + shell.length);
  check('events.js is in the cache — without it no button works offline',
        cached.some((u) => u === '/js/events.js'), cached.filter(u => u.startsWith('/js/')).slice(0, 6).join(', '));

  // ── Offline: the app must open ──────────────────────────────────────────
  await ctx.setOffline(true);
  const p2 = await ctx.newPage();
  const offlineErrors = [];
  p2.on('pageerror', (e) => offlineErrors.push(e.message));
  let opened = true;
  try {
    await p2.goto(BASE + '/', { waitUntil: 'domcontentloaded', timeout: 15000 });
  } catch (e) { opened = false; }
  check('offline the app opens from the cache', opened);
  if (opened) {
    check('offline app markup is in place',
          await p2.evaluate(() => !!document.getElementById('auth-screen') || !!document.getElementById('chat-screen')));
    check('offline handlers loaded',
          await p2.evaluate(() => typeof window.ACTIONS !== 'undefined' || typeof doAuth === 'function'));
  }
  await ctx.setOffline(false);
  await p2.close();

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? `\nFAILURES: ${failures}` : '\nall checks passed');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
