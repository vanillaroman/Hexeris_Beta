// Link previews: do not bother the server where the answer is predictable.
//
// ═══ WHAT WAS WRONG ═══════════════════════════════════════════════════════
//
// The client asked /unfurl for EVERY link in a conversation and repeated that on
// every page load. In the console it looked like dozens of red lines in a row,
// and on the server like just as many trips outside.
//
// And the server refused justly: a gpg key and a .deb package are not HTML,
// localhost and private addresses are rejected by the SSRF guard, and
// "http://{config.WEB_HOST}" is not an address at all but a piece of config from
// a forwarded snippet.
//
// ═══ WHERE THE LINE RUNS ═════════════════════════════════════════════════
//
// Guessing EVERYTHING on the client is neither necessary nor possible:
// "/linux/ubuntu/gpg" has no extension, and from the address alone it cannot be
// told apart from an ordinary page. So two mechanisms are at work, and the suite
// checks both:
//
//   1. the obviously hopeless is not asked about AT ALL (templates, loopback,
//      private ranges, names without a dot, file extensions);
//   2. everything else is asked ONCE, and the refusal is remembered — including
//      across the next page load.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : ''));
  if (!ok) failures++;
};
const tag = 'lp' + String(Date.now()).slice(-7);

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

// The addresses are taken from production logs, not invented.
const HOPELESS = [
  'http://localhost:8080/register',
  'http://app:8080',
  'http://127.0.0.1:8765',
  'http://192.168.0.16:8765',
  'https://$APP_DOMAIN/healthz',
  'http://{config.WEB_HOST}:{config.WEB_PORT}',
  'http://security.ubuntu.com/ubuntu/pool/main/g/gcc-14/libitm1_14.2.0_amd64.deb',
  'https://eapi.stalcraft.net/{region}/auction/{item_id}/lots',
];
// This address looks like an ordinary page and it MUST be visited — exactly once.
// There is no HTML there, the server will refuse, and repeating is pointless.
const ASK_ONCE = 'https://download.docker.com/linux/ubuntu/gpg';

(async () => {
  await waitReady();
  const me = 'me' + tag, bob = 'bob' + tag;
  await reg(me); const tBob = await reg(bob);
  await seed(tBob, [...HOPELESS, ASK_ONCE].map((u) => ({ type: 'message', to: me, body: 'link: ' + u })));

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
  const page = await ctx.newPage();
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));

  const asked = [];
  page.on('request', (r) => {
    const u = r.url();
    if (u.includes('/unfurl?')) {
      try { asked.push(decodeURIComponent(new URL(u).searchParams.get('url') || '')); } catch (e) {}
    }
  });
  // We do not go outside from the test: the response is stubbed. Otherwise the
  // suite would depend on whether the machine running it has internet.
  await page.route('**/unfurl?**', (route) => route.fulfill({ status: 422, body: 'not previewable' }));

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#auth-screen', { state: 'visible' });
  await page.fill('#auth-username', me);
  await page.fill('#auth-password', 'passw0rd-test');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(1500);
  await page.click(`#contact-${bob}`);
  await page.waitForTimeout(2500);

  // Control: there really are links in the conversation and preview slots were
  // created — otherwise "zero requests" would mean there is nothing to measure.
  const links = await page.evaluate(() => document.querySelectorAll('.msg-link').length);
  check('links in the conversation are recognised (control)', links >= HOPELESS.length, 'links ' + links);

  const hopelessAsked = asked.filter((u) => HOPELESS.includes(u));
  check('hopeless addresses are not asked about at all', hopelessAsked.length === 0,
        hopelessAsked.join(', '));
  check('an ordinary-looking address is asked exactly once',
        asked.filter((u) => u === ASK_ONCE).length === 1,
        'asked ' + asked.filter((u) => u === ASK_ONCE).length + ' times');

  // Reload: the refusal must survive it, otherwise every sign-in repeats the same
  // batch of requests and the same batch of red lines in the console.
  asked.length = 0;
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(1500);
  await page.click(`#contact-${bob}`);
  await page.waitForTimeout(2500);

  check('after a reload the refusal is not asked again', asked.length === 0,
        'asked again: ' + asked.join(', '));

  // The cards must not be left hanging as empty slabs.
  const emptySlots = await page.evaluate(() =>
    document.querySelectorAll('.lp-slot:empty').length);
  check('no empty preview slots left', emptySlots === 0, 'slots ' + emptySlots);

  check('no JS errors', pageErrors.length === 0, pageErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
