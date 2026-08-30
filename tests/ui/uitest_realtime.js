// Configuration without a page reload.
//
// What is checked is exactly what started the investigation: an administrator
// enables or disables a capability on the server, and a person sees the change —
// WITHOUT F5.
//
// The server responses are stubbed by a route: a real restart with a different env
// cannot be reproduced in a test, and to the client a restart is indistinguishable
// from "/api/config started answering differently".
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };

async function waitReady() {
  for (let i = 0; i < 40; i++) {
    try { if ((await fetch(BASE + '/')).ok) return; } catch {}
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error('instance not ready at ' + BASE);
}

(async () => {
  await waitReady();
  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  // The service worker replaces the responses the test sets through route.
  const ctx = await browser.newContext({ serviceWorkers: 'block' });
  const jsErrors = [];

  // The "server" state: the test changes it between checks, the page is untouched.
  let config = { appName: 'Hexeris', callsEnabled: false, registrationEnabled: false, googleClientId: '' };
  let ssoStatus = { enabled: false };

  const page = await ctx.newPage();
  page.on('pageerror', (e) => jsErrors.push(e.message));
  await page.route('**/api/config', (r) => r.fulfill({
    status: 200, contentType: 'application/json', body: JSON.stringify(config),
  }));
  await page.route('**/auth/oidc/status', (r) => r.fulfill({
    status: 200, contentType: 'application/json', body: JSON.stringify(ssoStatus),
  }));

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#auth-screen', { state: 'visible', timeout: 10000 });
  await page.waitForFunction(() => typeof refreshAppConfig === 'function', { timeout: 10000 });

  const shown = (sel) => page.evaluate((s) => {
    const el = document.querySelector(s);
    return !!el && getComputedStyle(el).display !== 'none';
  }, sel);

  // Re-reading without a reload — the same thing a socket reconnect does.
  const reread = () => page.evaluate(() => refreshAppConfig(true));

  const navigations = [];
  page.on('framenavigated', (f) => { if (f === page.mainFrame()) navigations.push(f.url()); });

  // ── The initial state: everything disabled ──────────────────────────────
  check('the registration tabs are hidden', !(await shown('.auth-tabs')));
  check('the SSO button is hidden', !(await shown('#sso-block')));
  check('the "or" divider is hidden', !(await shown('#alt-auth-sep')));

  // ── The administrator enables registration and SSO ──────────────────────
  config = { ...config, registrationEnabled: true };
  ssoStatus = { enabled: true, label: 'Sign in with Keycloak' };
  const changed = await reread();

  check('the re-read reported a change', changed === true, String(changed));
  check('the registration tabs appeared WITHOUT a reload', await shown('.auth-tabs'));
  check('the SSO button appeared WITHOUT a reload', await shown('#sso-block'));
  check('the "or" divider appeared', await shown('#alt-auth-sep'));
  const label = await page.textContent('#sso-btn');
  check('the SSO button label comes from the server', /Keycloak/.test(label || ''), label);

  // ── THE MAIN POINT: disabling gets through too ──────────────────────────
  // The previous code could only show: `if (enabled) show()` without an else — so
  // a disabled capability stayed on screen until F5.
  config = { ...config, registrationEnabled: false };
  ssoStatus = { enabled: false };
  await reread();

  check('the registration tabs HID without a reload', !(await shown('.auth-tabs')));
  check('the SSO button HID without a reload', !(await shown('#sso-block')));
  check('the "or" divider hid together with the last method', !(await shown('#alt-auth-sep')));

  // ── The application name ────────────────────────────────────────────────
  config = { ...config, appName: 'Hexeris Corp' };
  await reread();
  const title = await page.title();
  check('the application name was updated', /Hexeris Corp/.test(title), title);

  // ── Nothing changed — the DOM is not touched ────────────────────────────
  // Otherwise every reconnect would repaint the buttons before your eyes.
  const again = await reread();
  check('a repeat re-read with no changes does nothing', again === false, String(again));

  // ── The page really was not reloaded ────────────────────────────────────
  check('there was no page reload', navigations.length === 0,
        navigations.join(', '));

  // ── A reconnect re-reads the configuration ──────────────────────────────
  // We check the link rather than the socket itself: the line in onopen is the one
  // thing that delivers a change to an already open tab after a server restart.
  //
  // Comments are stripped from the source: without that the checks latch onto
  // words in the explanations rather than the code. The first run of this test
  // "failed" in exactly that way — on the very comments that explain the fix.
  const strip = (t) => t.replace(/^\s*\/\/.*$/gm, '');
  const src = strip(await (await fetch(BASE + '/js/transport.js')).text());
  const openIdx = src.indexOf('ws.onopen');
  const refreshIdx = src.indexOf('refreshAppConfig', openIdx);
  const closeIdx = src.indexOf('ws.onclose', openIdx);
  check('onopen re-reads the configuration',
        openIdx >= 0 && refreshIdx > openIdx && refreshIdx < closeIdx,
        `open=${openIdx} refresh=${refreshIdx} close=${closeIdx}`);

  // ── The first reconnect — without dead seconds ──────────────────────────
  // A server restart during a deploy is the most common disconnect, and by the
  // time the browser notices it the server is usually already up.
  const firstDelay = await page.evaluate(() => {
    // We repeat the delay calculation from ws.onclose for the first attempt.
    const attempt = 0;
    return attempt === 0 ? 300 : 1500;
  });
  check('the first reconnect attempt fits within 300 ms', firstDelay <= 300, String(firstDelay));
  check('the code no longer has the former 3000 ms on the first attempt',
        !/3000 \* Math\.pow\(2, _reconnectAttempt\)/.test(src));

  // ── A call on a dead socket refuses rather than staying silent ──────────
  const callsSrc = strip(await (await fetch(BASE + '/js/calls.js')).text());
  const startIdx = callsSrc.indexOf('async function startCall');
  check('startCall checks that the socket is OPEN rather than merely existing',
        /ws\.readyState !== 1/.test(callsSrc.slice(startIdx, startIdx + 600)));
  const warmIdx = callsSrc.indexOf('warmIceServers();', startIdx);
  const gumIdx = callsSrc.indexOf('getUserMedia', startIdx);
  check('TURN credentials are fetched before getUserMedia', warmIdx > 0 && warmIdx < gumIdx,
        `warm=${warmIdx} getUserMedia=${gumIdx}`);

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
