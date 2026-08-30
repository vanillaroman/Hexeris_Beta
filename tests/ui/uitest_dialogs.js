// Application windows: stillness, readability in both themes, fitting the screen.
//
// The suite was written from three concrete breakages found by measurement, not
// "just in case":
//
//   1. The 2FA window travelled 154px vertically between steps. It is centred,
//      while the step heights are 483 → 543 → 356 → 235, so the title moved on
//      every transition. To a person that reads as "the windows drift".
//   2. In the light theme the dialogs were unreadable: .hex-modal set the
//      background as the literal rgba(22,23,29,.72), bypassing the --glass-*
//      tokens, which gave a dark background with dark text. The measured contrast
//      was 1.06 against the WCAG AA requirement of 4.5. It affected six windows
//      at once.
//   3. The windows had four widths: 340, 360, 380 and 420 — in one application.
//
// The 2FA steps are walked FOR REAL: the code is computed from the secret the
// server issued. Faking the "recovery codes" step would show an empty grid and
// would miss the very thing the suite was written for.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const crypto = require('crypto');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : ''));
  if (!ok) failures++;
};
const tag = 'dlg' + String(Date.now()).slice(-7);

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

// WebSocket has been built into Node since version 22 — no separate package needed.
const seed = (token, msgs) => new Promise((res, rej) => {
  const s = new WebSocket(BASE.replace('http', 'ws') + '/ws?token=' + token);
  s.onopen = () => {
    msgs.forEach((m, i) => s.send(JSON.stringify({ ...m, id: tag + i })));
    setTimeout(() => { s.close(); res(); }, 700);
  };
  s.onerror = rej;
});

// ── TOTP per RFC 6238, so the real enabling scenario can be walked ────────
function b32decode(s) {
  const A = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = 0, value = 0; const out = [];
  for (const c of s.replace(/=+$/, '').toUpperCase()) {
    const i = A.indexOf(c);
    if (i < 0) continue;
    value = (value << 5) | i; bits += 5;
    if (bits >= 8) { out.push((value >>> (bits - 8)) & 0xff); bits -= 8; }
  }
  return Buffer.from(out);
}
function totp(secret) {
  const buf = Buffer.alloc(8);
  buf.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const h = crypto.createHmac('sha1', b32decode(secret)).update(buf).digest();
  const o = h[h.length - 1] & 0xf;
  const code = ((h[o] & 0x7f) << 24 | h[o + 1] << 16 | h[o + 2] << 8 | h[o + 3]) % 1e6;
  return String(code).padStart(6, '0');
}

// Text-to-background contrast per WCAG. The window's translucent background is
// composited over the page ground — otherwise the glass would be counted as
// darker than it looks.
const contrastOf = (page, sel) => page.evaluate((s) => {
  const el = document.querySelector(s);
  if (!el) return null;
  const nums = (c) => (c.match(/[\d.]+/g) || []).map(Number);
  const over = (fg, bg) => {
    const a = fg.length > 3 ? fg[3] : 1;
    return [0, 1, 2].map((i) => fg[i] * a + bg[i] * (1 - a));
  };
  const lum = ([r, g, b]) => {
    const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
  };
  const page_ = nums(getComputedStyle(document.body).backgroundColor);
  const bg = over(nums(getComputedStyle(el).backgroundColor), page_.length >= 3 ? page_ : [0, 0, 0]);
  const fg = over(nums(getComputedStyle(el).color), bg);
  const L1 = lum(bg), L2 = lum(fg);
  return +(((Math.max(L1, L2) + 0.05) / (Math.min(L1, L2) + 0.05)).toFixed(2));
}, sel);

const rectOf = (page, sel) => page.evaluate((s) => {
  const el = document.querySelector(s);
  if (!el) return null;
  const r = el.getBoundingClientRect();
  return { top: Math.round(r.top), bottom: Math.round(r.bottom),
           left: Math.round(r.left), right: Math.round(r.right),
           w: Math.round(r.width), h: Math.round(r.height) };
}, sel);

async function login(page, user) {
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#auth-screen', { state: 'visible' });
  await page.fill('#auth-username', user);
  await page.fill('#auth-password', 'passw0rd-test');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 15000 });
  await page.waitForTimeout(900);
}

(async () => {
  await waitReady();
  // Different users per section, deliberately: the first section turns the second
  // factor on for real, and that user can no longer sign in without a code.
  const me2fa = 'two' + tag, me = 'ann' + tag, bob = 'bob' + tag;
  await reg(me2fa); const tMe = await reg(me); const tBob = await reg(bob);
  // A conversation is needed by the Escape section: without a single contact the
  // list-menu and other-profile checks were quietly skipped, and the suite
  // "passed" having checked nothing.
  await seed(tBob, [{ type: 'message', to: me, body: 'Morning — did you look at the draft?' }]);
  await seed(tMe, [{ type: 'message', to: bob, body: 'Reading it now.' }]);

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  const jsErrors = [];

  // ══ 1. 2FA: the window is still on every step of a real scenario ═══════
  {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push('2fa: ' + e.message));
    await login(page, me2fa);

    const tops = [];
    const note = async (label) => {
      await page.waitForTimeout(350);
      const r = await rectOf(page, '#twofa-modal-box');
      tops.push({ label, top: r && r.top, h: r && r.h });
      return r;
    };

    await page.evaluate(() => open2FASetup());
    await note('loading');
    await page.waitForSelector('#twofa-setup-code', { timeout: 10000 });
    await note('setup');

    // The secret is on screen — we compute the code from it, exactly as a person
    // with a phone would.
    const secret = (await page.textContent('.dlg-code') || '').trim();
    check('the secret for manual entry is shown', /^[A-Z2-7]{16,}$/.test(secret), secret.slice(0, 8) + '…');

    // An input error: the block grows, but the title must not shift.
    await page.fill('#twofa-setup-code', '000000');
    await page.click('[data-act="confirm2FAEnable"]');
    await page.waitForTimeout(800);
    await note('code error');
    const errVisible = await page.evaluate(() => {
      const e = document.getElementById('twofa-modal-error');
      return !!e && getComputedStyle(e).display !== 'none' && e.textContent.trim().length > 0;
    });
    check('a wrong code is explained in place', errVisible);

    // The real code.
    await page.fill('#twofa-setup-code', totp(secret));
    await page.click('[data-act="confirm2FAEnable"]');
    await page.waitForSelector('.dlg-codes', { timeout: 10000 });
    await note('recovery codes');
    const codes = await page.evaluate(() =>
      [...document.querySelectorAll('.dlg-codes > div')].map((d) => d.textContent.trim()).filter(Boolean));
    check('the recovery codes really are shown', codes.length >= 5, 'there are ' + codes.length);

    // Escape on the codes step must not close the window: close2FAModal wipes the
    // codes, they are shown exactly once and cannot be brought back — what is left
    // is an enabled second factor without a single backup code. There is an
    // explicit "Done" button right there, and that is what must be pressed.
    await page.keyboard.press('Escape');
    await page.waitForTimeout(400);
    const stillOpen = await page.evaluate(() => {
      const o = document.getElementById('twofa-modal');
      return !!o && getComputedStyle(o).display !== 'none' &&
             document.querySelectorAll('.dlg-codes > div').length > 0;
    });
    check('Escape does not wipe the recovery codes', stillOpen);

    await page.evaluate(() => start2FADisable());
    await note('turning off');

    // And on an ordinary step Escape does close — there is nothing to lose there.
    await page.keyboard.press('Escape');
    await page.waitForTimeout(400);
    const closed = await page.evaluate(() => {
      const o = document.getElementById('twofa-modal');
      return !o || getComputedStyle(o).display === 'none';
    });
    check('on the other steps Escape closes the window', closed);
    await page.evaluate(() => { if (typeof open2FASetup === 'function') {} });

    const valid = tops.filter((t) => t.top !== null && t.top !== undefined);
    const drift = Math.max(...valid.map((t) => t.top)) - Math.min(...valid.map((t) => t.top));
    check('the 2FA window does not shift between steps', drift === 0,
          'spread ' + drift + 'px: ' + valid.map((t) => `${t.label}=${t.top}`).join(', '));
    // Negative control: the step heights really do differ, so the stillness comes
    // from pinning rather than from there being nothing to measure.
    const heights = new Set(valid.map((t) => t.h));
    check('the step heights do differ (control)', heights.size >= 3,
          'distinct heights: ' + heights.size);

    await ctx.close();
  }

  // ══ 2. Window readability in both themes ════════════════════════════════
  for (const theme of ['dark', 'light']) {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
    await ctx.addInitScript((t) => { try { localStorage.setItem('hc_theme', t); } catch {} }, theme);
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push(theme + ': ' + e.message));
    await login(page, me);

    const applied = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
    check(`the ${theme} theme really was applied`,
          theme === 'light' ? applied === 'light' : applied === null, 'data-theme=' + applied);

    await page.evaluate(() => openMyProfile());
    await page.waitForTimeout(600);
    const cProfile = await contrastOf(page, '.profile-modal');
    check(`the profile is readable in the ${theme} theme`, cProfile !== null && cProfile >= 4.5,
          'contrast ' + cProfile);
    await page.keyboard.press('Escape');
    await page.waitForTimeout(300);

    await page.evaluate(() => open2FASetup());
    await page.waitForTimeout(900);
    const cBox = await contrastOf(page, '#twofa-modal-box');
    check(`the 2FA window is readable in the ${theme} theme`, cBox !== null && cBox >= 4.5, 'contrast ' + cBox);
    await page.evaluate(() => close2FAModal());

    await ctx.close();
  }

  // ══ 2b. Escape closes ALL transient windows, not just some ════════════
  //
  // The handler dismissed #ctx-menu (the message menu) but not #chat-ctx-menu
  // (the chat list menu, on right-click and long press): it survived the opening
  // of the next window, and both the menu and the card on top of it ended up on
  // screen. Another person's profile and forwarding did not close at all.
  {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push('escape: ' + e.message));
    await login(page, me);

    const visible = (sel) => page.evaluate((s) => {
      const el = document.querySelector(s);
      if (!el) return false;
      const cs = getComputedStyle(el);
      return cs.display !== 'none' && cs.visibility !== 'hidden' && +cs.opacity > 0.01;
    }, sel);

    check('there is a contact to check with (control)', (await page.locator('.contact-item').count()) > 0);
    if (await page.locator('.contact-item').count()) {
      await page.click('.contact-item', { button: 'right' });
      await page.waitForTimeout(400);
      check('the chat list menu opened (control)', await visible('#chat-ctx-menu'));
      await page.keyboard.press('Escape');
      await page.waitForTimeout(400);
      check('Escape closes the chat list menu', !(await visible('#chat-ctx-menu')));

      const peer = await page.evaluate(() => document.querySelector('.contact-item').dataset.peer);
      await page.evaluate((u) => openPeerProfile(u), peer);
      await page.waitForTimeout(700);
      check('another person\'s profile opened (control)', await visible('#peer-profile-overlay'));
      await page.keyboard.press('Escape');
      await page.waitForTimeout(400);
      check('Escape closes another person\'s profile', !(await visible('#peer-profile-overlay')));
    }
    await ctx.close();
  }

  // ══ 3. One width for every window ══════════════════════════════════════
  {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push('widths: ' + e.message));
    await login(page, me);

    const widths = {};
    const open = async (label, fn, sel) => {
      await page.evaluate(fn);
      await page.waitForTimeout(600);
      const r = await rectOf(page, sel);
      if (r) widths[label] = r.w;
      await page.keyboard.press('Escape');
      await page.evaluate(() => { if (typeof close2FAModal === 'function') close2FAModal(); });
      await page.waitForTimeout(300);
    };
    await open('profile', () => openMyProfile(), '.profile-modal');
    await open('2FA', () => open2FASetup(), '#twofa-modal-box');
    await open('new group', () => openGroupModal(), '#group-modal .group-box');
    await open('network test', () => openNetworkTest(), '.nettest-modal');

    const uniq = new Set(Object.values(widths));
    check('every window has the same width', uniq.size === 1,
          Object.entries(widths).map(([k, v]) => `${k}=${v}`).join(', '));
    await ctx.close();
  }

  // ══ 4. The window fits the screen, including a phone in landscape ══════
  for (const vp of [
    { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true },
    { name: 'phone in landscape', width: 844, height: 390, isMobile: true, hasTouch: true },
  ]) {
    const { name, ...viewport } = vp;
    const ctx = await browser.newContext({
      viewport: { width: viewport.width, height: viewport.height },
      isMobile: viewport.isMobile, hasTouch: viewport.hasTouch, serviceWorkers: 'block',
    });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => jsErrors.push(name + ': ' + e.message));
    await login(page, me);
    await page.evaluate(() => open2FASetup());
    await page.waitForTimeout(1100);
    const r = await rectOf(page, '#twofa-modal-box');
    const vh = viewport.height, vw = viewport.width;
    check(`${name}: the window does not run off the screen`,
          r && r.top >= 0 && r.bottom <= vh && r.left >= 0 && r.right <= vw,
          JSON.stringify(r) + ' screen ' + vw + 'x' + vh);
    // If it does not fit, it must scroll inside, otherwise the "Turn on" button
    // ends up out of reach.
    const scrollable = await page.evaluate(() => {
      const el = document.getElementById('twofa-modal-box');
      return !el || el.scrollHeight <= el.clientHeight + 1 ||
             getComputedStyle(el).overflowY === 'auto';
    });
    check(`${name}: the window content is reachable`, scrollable);
    await ctx.close();
  }

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
