// The second sign-in step: the code screen.
//
// There is no real second factor here — the server responses are stubbed by a
// route. What is checked is exactly what does not depend on the server and what
// breaks the protection most quietly: whether a session is saved BEFORE the code,
// whether the ticket settles in localStorage, whether the reason for a refusal
// gets through, and whether it is possible to leave this screen at all.
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

const TICKET = 'ticket-under-test-0001';

(async () => {
  await waitReady();
  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  // The service worker intercepts same-origin GET and replaces the responses the
  // test stubs through route (see uitest_sso.js).
  const ctx = await browser.newContext({ serviceWorkers: 'block' });
  const jsErrors = [];

  const newPage = async () => {
    const p = await ctx.newPage();
    p.on('pageerror', (e) => jsErrors.push(e.message));
    // The password is always "accepted", and the server always asks for a second factor.
    await p.route('**/login', (r) => r.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ twofa_required: true, ticket: TICKET, username: 'ivanov' }),
    }));
    return p;
  };

  const signIn = async (p) => {
    await p.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#auth-screen', { state: 'visible', timeout: 10000 });
    await p.fill('#auth-username', 'ivanov');
    await p.fill('#auth-password', 'whatever');
    await p.click('#auth-btn');
    await p.waitForSelector('#twofa-screen', { state: 'visible', timeout: 10000 });
  };

  const shown = (p, sel) => p.evaluate((s) => {
    const el = document.querySelector(s);
    return !!el && getComputedStyle(el).display !== 'none';
  }, sel);

  // ── The password is accepted: the code is asked for, no session is created ─
  {
    const p = await newPage();
    await signIn(p);

    check('the code screen is shown after the password', await shown(p, '#twofa-screen'));
    check('the sign-in screen is hidden', !(await shown(p, '#auth-screen')));

    const acc = await p.textContent('#twofa-account');
    check('the account the code is for is named', (acc || '').includes('ivanov'), acc);

    // THE MAIN POINT: before the code there must be no session in any form.
    const stored = await p.evaluate(() => JSON.stringify(Object.entries(localStorage)));
    check('no token is saved before the second factor', !/token/i.test(stored), stored.slice(0, 200));
    check('the ticket did not settle in localStorage', !stored.includes(TICKET), stored.slice(0, 200));

    const tok = await p.evaluate(() => (typeof token === 'undefined' ? null : token));
    check('the token variable is empty before the second factor', !tok, String(tok));

    await p.close();
  }

  // ── A wrong code: the reason gets through, the screen stays ─────────────
  {
    const p = await newPage();
    await p.route('**/auth/2fa/verify', (r) => r.fulfill({
      status: 401, contentType: 'text/plain', body: 'wrong code\n',
    }));
    await signIn(p);
    await p.fill('#twofa-code', '000000');
    await p.click('#twofa-btn');
    await p.waitForTimeout(300);

    const err = (await p.textContent('#twofa-error')) || '';
    check('the reason for the refusal is shown verbatim', /wrong code/i.test(err), err);
    check('after the refusal we stay on the code screen', await shown(p, '#twofa-screen'));
    check('the chat did not open', !(await shown(p, '#chat-screen')));

    // The button must return to a working state — otherwise there is simply no
    // second attempt, and the person reloads the page, losing the ticket.
    const disabled = await p.getAttribute('#twofa-btn', 'disabled');
    check('the button is clickable again', disabled === null, String(disabled));

    await p.close();
  }

  // ── The ticket died: we go back to the sign-in screen rather than stay silent ─
  {
    const p = await newPage();
    await p.route('**/auth/2fa/verify', (r) => r.fulfill({
      status: 401, contentType: 'text/plain', body: 'this sign-in attempt has expired — enter your password again',
    }));
    await signIn(p);
    await p.fill('#twofa-code', '123456');
    await p.click('#twofa-btn');
    await p.waitForTimeout(300);

    check('after the ticket died we are back on the sign-in screen', await shown(p, '#auth-screen'));
    check('the code screen is closed', !(await shown(p, '#twofa-screen')));
    const err = (await p.textContent('#auth-error')) || '';
    check('the sign-in screen says why', /expired/i.test(err), err);
    await p.close();
  }

  // ── The correct code: the session appears only now ──────────────────────
  {
    const p = await newPage();
    await p.route('**/auth/2fa/verify', (r) => r.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ token: 'jwt-after-2fa', username: 'ivanov' }),
    }));
    await signIn(p);
    await p.fill('#twofa-code', '123456');
    await p.click('#twofa-btn');
    await p.waitForTimeout(500);

    check('the code screen is closed', !(await shown(p, '#twofa-screen')));
    const tok = await p.evaluate(() => (typeof token === 'undefined' ? null : token));
    check('the token arrives after the code', tok === 'jwt-after-2fa', String(tok));
    await p.close();
  }

  // ── Going back throws the ticket away ───────────────────────────────────
  {
    const p = await newPage();
    await signIn(p);
    await p.click('#twofa-back');
    await p.waitForTimeout(200);

    check('the "back" button returned the sign-in screen', await shown(p, '#auth-screen'));
    check('the code screen is closed', !(await shown(p, '#twofa-screen')));
    const pw = await p.inputValue('#auth-password');
    check('the password field is cleared', pw === '', pw);

    // The ticket must not survive going back: otherwise it stays in the tab's
    // memory as a pass to enter a code without a password.
    const held = await p.evaluate(() => (typeof _twofaTicket === 'undefined' ? 'undef' : _twofaTicket));
    check('the ticket was thrown away', !held || held === 'undef', String(held));
    await p.close();
  }

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
