// The sign-in screen: one divider for all the methods, a Google slab without a
// light ground, a form that does not jump when the button appears.
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
  const ctx = await browser.newContext({ serviceWorkers: 'block' });
  const jsErrors = [];

  const open = async (cfg, sso) => {
    const p = await ctx.newPage();
    p.on('pageerror', (e) => jsErrors.push(e.message));
    await p.route('**/api/config', (r) => r.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify(cfg),
    }));
    await p.route('**/auth/oidc/status', (r) => r.fulfill({
      status: 200, contentType: 'application/json', body: JSON.stringify(sso),
    }));
    // GSI is not let out: the test must not depend on the external network.
    await p.route('https://accounts.google.com/**', (r) => r.fulfill({ status: 200, contentType: 'application/javascript', body: '' }));
    await p.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#auth-btn', { state: 'visible', timeout: 15000 });
    // Wait longer than the GSI safety timer (1.5 s): otherwise the "the block
    // appeared" check catches the moment before it fires.
    await p.waitForTimeout(2000);
    return p;
  };

  const sepCount = (p) => p.evaluate(() =>
    [...document.querySelectorAll('#auth-screen .auth-sep')]
      .filter((n) => getComputedStyle(n).display !== 'none').length);

  // ── Both methods enabled: ONE divider ───────────────────────────────────
  {
    const p = await open({ googleClientId: 'x.apps.googleusercontent.com' }, { enabled: true, label: 'Sign in with SSO' });
    check('both methods enabled — one divider', (await sepCount(p)) === 1, 'found: ' + (await sepCount(p)));
    check('the SSO button is visible', await p.isVisible('#sso-btn'));
    check('the Google block is visible', await p.isVisible('#google-auth-block'));
    await p.close();
  }

  // ── SSO only ────────────────────────────────────────────────────────────
  {
    const p = await open({}, { enabled: true });
    check('SSO only — there is a divider and only one', (await sepCount(p)) === 1);
    check('the Google block is hidden', !(await p.isVisible('#google-auth-block')));
    await p.close();
  }

  // ── Nothing extra enabled: there must be no divider ────────────────────
  {
    const p = await open({}, { enabled: false });
    check('with no alternative methods there is no divider', (await sepCount(p)) === 0);
    check('the password form is in place', await p.isVisible('#auth-btn'));
    await p.close();
  }

  // ── The Google slab: a dark frame, the space reserved ─────────────────────
  {
    const p = await open({ googleClientId: 'x.apps.googleusercontent.com' }, { enabled: false });
    const box = await p.evaluate(() => {
      const n = document.querySelector('#google-auth-block .g_id_signin');
      if (!n) return null;
      const cs = getComputedStyle(n);
      return { bg: cs.backgroundColor, radius: cs.borderRadius, overflow: cs.overflow, minH: parseInt(cs.minHeight, 10) };
    });
    check('the button container exists', box !== null);
    if (box) {
      check('the ground is not white', box.bg !== 'rgb(255, 255, 255)' && box.bg !== 'rgba(0, 0, 0, 0)', box.bg);
      check('the corners are rounded — the light edges are cut off', parseInt(box.radius, 10) > 0, box.radius);
      check('overflow is hidden, otherwise the frame does not work', box.overflow === 'hidden', box.overflow);
      check('the height is reserved — the form will not jump', box.minH >= 40, String(box.minH));
    }

    // The block must appear even if GSI never arrived: the safety timer.
    const shown = await p.evaluate(() => getComputedStyle(document.getElementById('google-auth-block')).opacity);
    check('the block appears even when GSI did not render the button', Number(shown) === 1, 'opacity=' + shown);
    await p.close();
  }

  // ── The invalidation tag on media ───────────────────────────────────────
  //
  // Some clients have stale 404s for /files/ stuck in their cache (error responses
  // went out with max-age=86400 for a while). The tag changes the address and the
  // browser re-requests the media — otherwise a server-side fix looks from the
  // outside like "nothing changed".
  {
    const p = await ctx.newPage();
    p.on('pageerror', (e) => jsErrors.push('bust: ' + e.message));
    await p.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#auth-btn', { state: 'visible', timeout: 15000 });

    const r = await p.evaluate(() => ({
      img: mediaSrc('/files/abc.gif'),
      // The fragment with the real file name must fall away: it is for the
      // interface, not for the server.
      withName: mediaSrc('/files/abc.gif#%D1%84%D0%BE%D1%82%D0%BE.gif'),
      empty: mediaSrc(''),
    }));
    check('an invalidation tag is added to the media address', /\/files\/abc\.gif\?v=\d+$/.test(r.img), r.img);
    check('the fragment with the file name does not reach the address',
          !r.withName.includes('#') && /\?v=\d+$/.test(r.withName), r.withName);
    check('an empty body does not become a tagged address', r.empty === '', JSON.stringify(r.empty));
    await p.close();
  }

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));
  await browser.close();
  console.log(failures ? `\nFAILURES: ${failures}` : '\nall checks passed');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
