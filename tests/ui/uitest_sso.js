// Sign-in through a corporate provider: the button, the return, the errors.
//
// The provider itself is not here — what is checked is what does not depend on
// it: whether the button is shown only when SSO is configured, whether the reason
// for a refusal reaches the person, whether the token ends up in the address bar
// (it must not) and whether the session is picked up after the return.
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
  // The service worker in this app intercepts EVERY same-origin GET
  // (network-first, see web/sw.js). For page checks it only gets in the way: its
  // cache replaces the responses the test stubs through route, and a failure
  // looks like an interface defect. Production is unaffected — there
  // network-first honestly goes to the network.
  const ctx = await browser.newContext({ serviceWorkers: 'block' });
  const jsErrors = [];
  const csp = [];

  const newPage = async () => {
    const p = await ctx.newPage();
    p.on('pageerror', (e) => jsErrors.push(e.message));
    p.on('console', (m) => { if (/Content Security Policy|Refused to/i.test(m.text())) csp.push(m.text()); });
    return p;
  };

  // ── SSO disabled: there must be no button ───────────────────────────────
  {
    const p = await newPage();
    await p.route('**/auth/oidc/status', (r) => r.fulfill({
      status: 200, contentType: 'application/json', body: '{"enabled":false}',
    }));
    await p.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#auth-btn', { state: 'visible', timeout: 15000 });
    await p.waitForTimeout(400);
    check('with SSO disabled there is no button', !(await p.isVisible('#sso-btn')));
    await p.close();
  }

  // ── SSO enabled: the button exists and leads to the provider ──────────────
  {
    const p = await newPage();
    await p.route('**/auth/oidc/status', (r) => r.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ enabled: true, label: 'Sign in with Keycloak' }),
    }));
    let started = 0;
    await p.route('**/auth/oidc/start', (r) => { started++; r.fulfill({ status: 200, contentType: 'text/html', body: 'idp' }); });
    await p.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#sso-btn', { state: 'visible', timeout: 15000 });
    check('the SSO button is shown', await p.isVisible('#sso-btn'));
    check('the button label comes from the server',
          (await p.textContent('#sso-btn')).includes('Keycloak'),
          await p.textContent('#sso-btn'));
    await p.click('#sso-btn');
    await p.waitForTimeout(600);
    check('pressing it goes to the server start endpoint', started === 1, 'navigations: ' + started);
    await p.close();
  }

  // ── Return with an error: the reason is visible, no sign-in happened ──────
  {
    const p = await newPage();
    await p.route('**/auth/oidc/status', (r) => r.fulfill({
      status: 200, contentType: 'application/json', body: '{"enabled":true}',
    }));
    await p.goto(BASE + '/?sso_error=' + encodeURIComponent('this email domain is not allowed to sign in here'),
                 { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#auth-btn', { state: 'visible', timeout: 15000 });
    await p.waitForTimeout(500);
    check('the reason for the refusal is shown verbatim',
          (await p.textContent('#auth-error')).includes('not allowed'),
          await p.textContent('#auth-error'));
    check('the address is cleared of parameters', !p.url().includes('sso_error'), p.url());
    check('no session was created', (await p.evaluate(() => localStorage.getItem('hc_token'))) === null);
    await p.close();
  }

  // ── Return with a code: exchange and sign-in ──────────────────────────────
  {
    const p = await newPage();
    let sentCode = null;
    await p.route('**/auth/oidc/status', (r) => r.fulfill({
      status: 200, contentType: 'application/json', body: '{"enabled":true}',
    }));
    await p.route('**/auth/oidc/exchange', (r) => {
      sentCode = JSON.parse(r.request().postData() || '{}').code;
      r.fulfill({ status: 200, contentType: 'application/json', body: '{"token":"jwt-abc","username":"grace"}' });
    });
    // After this the app follows its usual path and hits /history — we stub it so
    // the session check does not depend on live data.
    await p.route('**/history*', (r) => r.fulfill({ status: 200, contentType: 'application/json', body: '[]' }));
    await p.goto(BASE + '/?sso=one-time-code-123', { waitUntil: 'domcontentloaded' });
    await p.waitForFunction(() => localStorage.getItem('hc_token') !== null, null, { timeout: 15000 }).catch(() => {});

    check('the one-time code was sent for exchange', sentCode === 'one-time-code-123', String(sentCode));
    check('the token is saved in the session',
          (await p.evaluate(() => localStorage.getItem('hc_token'))) === 'jwt-abc');
    check('the username is saved',
          (await p.evaluate(() => localStorage.getItem('hc_user'))) === 'grace');
    check('the address is cleaned — the code did not stay in history',
          !p.url().includes('sso='), p.url());
    await p.close();
  }

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));
  check('no CSP violations', csp.length === 0, csp.join(' | '));

  await browser.close();
  console.log(failures ? `\nFAILURES: ${failures}` : '\nall checks passed');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
