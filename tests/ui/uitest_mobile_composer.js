// The mobile composer: four fixes from a user report.
//
//  1. The send button must not "stick" grey after a tap (:hover on touch).
//  2. On a phone the input panel has no glass — it is just a solid background.
//  3. While the field is empty there is no send button, a microphone instead.
//  4. A tap on </> does not raise the keyboard (does not focus the textarea).
//
// Controls are mandatory: almost every check here would pass on broken code too
// unless we make sure "before" differs from "after". So we measure the button
// colour BEFORE and AFTER the tap, and visibility on an empty and a non-empty
// field.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');

const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);

let failures = 0;
function check(name, ok, extra) {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + name + (extra ? '  — ' + extra : ''));
  if (!ok) failures++;
}

async function register(username) {
  const r = await fetch(BASE + '/register', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password: 'Password123!' })
  });
  if (!r.ok) throw new Error('register: ' + r.status);
  return (await r.json()).token;
}

(async () => {
  const alice = U('mc_alice'), bob = U('mc_bob');
  await register(alice);
  const bobTok = await register(bob);

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const ctx = await browser.newContext({ ...devices['Pixel 7'] });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', alice);
  await page.fill('#auth-password', 'Password123!');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  // An open chat is needed, otherwise there is no composer on screen.
  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bobTok}`);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  ws.send(JSON.stringify({ type: 'message', id: String(Date.now()), from: bob, to: alice, body: 'hi' }));
  await new Promise(r => setTimeout(r, 600));
  ws.close();
  await page.waitForSelector(`#contact-${bob}`, { timeout: 8000 });
  await page.click(`#contact-${bob}`);
  await page.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 300));

  const css = (sel, prop) => page.evaluate(([s, p]) =>
    getComputedStyle(document.querySelector(s)).getPropertyValue(p), [sel, prop]);
  const shown = (sel) => page.evaluate((s) => {
    const el = document.querySelector(s);
    return !!el && getComputedStyle(el).display !== 'none';
  }, sel);

  // ── 3. Empty field: no send button, the microphone is there ───────────────
  check('empty: send hidden', (await shown('.input-area .send-btn')) === false);
  check('empty: mic visible', (await shown('.input-area .mic-btn')) === true);

  // ── 2. The input panel is translucent on a phone too ──────────────────────
  const bf = (await css('.input-area', 'backdrop-filter')) ||
             (await css('.input-area', '-webkit-backdrop-filter'));
  // The requirement changed: on a phone the panel must be fully transparent so
  // the message text underneath it is readable.
  check('mobile: composer panel has no blur at all',
        !bf || bf === 'none', 'backdrop-filter=' + JSON.stringify(bf));
  const bg = await css('.input-area', 'background-color');
  check('mobile: input-area has no background of its own',
        bg === 'rgba(0, 0, 0, 0)', 'bg=' + bg);
  // Control: readability is held by the input field, not by the panel.
  const taBg = await css('#msg-textarea', 'background-color');
  check('control: the field itself stays opaque',
        taBg !== 'rgba(0, 0, 0, 0)' && !/rgba\([^)]*,\s*0?\.\d+\s*\)/.test(taBg), 'textarea bg=' + taBg);

  // ── The buttons are round and level with the field ────────────────────────
  const geom = await page.evaluate(() => {
    const g = (s) => { const e = document.querySelector(s); const r = e.getBoundingClientRect();
      return { h: +r.height.toFixed(1), w: +r.width.toFixed(1), bottom: +r.bottom.toFixed(1),
               radius: getComputedStyle(e).borderRadius }; };
    return { attach: g('#attach-btn'), ta: g('#msg-textarea'), mic: g('.input-area .mic-btn') };
  });
  check('mobile: attach button is a circle',
        geom.attach.radius === '50%' && geom.attach.w === geom.attach.h,
        JSON.stringify(geom.attach));
  check('mobile: attach button matches field height (±2px)',
        Math.abs(geom.attach.h - geom.ta.h) <= 2, geom.attach.h + ' vs ' + geom.ta.h);
  check('mobile: attach button sits on the same baseline as the field',
        Math.abs(geom.attach.bottom - geom.ta.bottom) <= 1,
        geom.attach.bottom + ' vs ' + geom.ta.bottom);
  check('mobile: mic button is a circle too', geom.mic.radius === '50%', geom.mic.radius);

  // ── The scrollbar in the field is hidden ──────────────────────────────────
  const sb = await page.evaluate(() => {
    const ta = document.getElementById('msg-textarea');
    ta.value = 'a\n'.repeat(30);
    ta.dispatchEvent(new Event('input', { bubbles: true }));
    const before = ta.offsetWidth - ta.clientWidth;   // the space taken by the bar
    const sw = getComputedStyle(ta).scrollbarWidth;
    const overflows = ta.scrollHeight > ta.clientHeight;
    ta.value = ''; ta.dispatchEvent(new Event('input', { bubbles: true }));
    return { gutter: before, scrollbarWidth: sw, overflows };
  });
  check('control: the field actually overflows (otherwise the check is empty)',
        sb.overflows === true, JSON.stringify(sb));
  check('mobile: no scrollbar gutter in the field',
        sb.gutter <= 2, 'gutter=' + sb.gutter + 'px scrollbar-width=' + sb.scrollbarWidth);

  // A wider field: the textarea must take up most of the panel width.
  const widthShare = await page.evaluate(() => {
    const a = document.querySelector('.input-area').getBoundingClientRect();
    const t = document.querySelector('#msg-textarea').getBoundingClientRect();
    return t.width / a.width;
  });
  check('mobile: textarea takes >60% of composer width',
        widthShare > 0.6, (widthShare * 100).toFixed(1) + '%');

  // ── 3b. Text appeared → send is there, the microphone is not ──────────────
  await page.fill('#msg-textarea', 'hello');
  await page.dispatchEvent('#msg-textarea', 'input');
  await new Promise(r => setTimeout(r, 120));
  check('typed: send visible', (await shown('.input-area .send-btn')) === true);
  check('typed: mic hidden', (await shown('.input-area .mic-btn')) === false);

  // A space is not text: the button must hide again (the check trims).
  await page.fill('#msg-textarea', '   ');
  await page.dispatchEvent('#msg-textarea', 'input');
  await new Promise(r => setTimeout(r, 120));
  check('whitespace-only: send hidden again', (await shown('.input-area .send-btn')) === false);

  await page.fill('#msg-textarea', 'hello');
  await page.dispatchEvent('#msg-textarea', 'input');
  await new Promise(r => setTimeout(r, 120));

  // ── 1. A stuck :hover on a touch screen ───────────────────────────────────
  // Checking the colour after a tap is useless: the send button hides together
  // with the send and loses :hover by itself when shown again — the test would
  // pass on broken CSS too. We ask the CSSOM itself: is there a :hover rule for
  // the composer buttons OUTSIDE a media (hover: hover). Every such rule stays
  // active on a phone after a tap.
  const strayHovers = await page.evaluate(() => {
    const TARGETS = ['.send-btn', '.mic-btn', '.attach-btn', '.fmt-btn'];
    const bad = [];
    const walk = (rules, guarded) => {
      for (const r of rules) {
        if (r.type === CSSRule.MEDIA_RULE) {
          const g = guarded || /hover:\s*hover/.test(r.conditionText || r.media.mediaText);
          walk(r.cssRules, g);
        } else if (r.type === CSSRule.SUPPORTS_RULE) {
          walk(r.cssRules, guarded);
        } else if (r.selectorText && r.selectorText.includes(':hover') && !guarded) {
          if (TARGETS.some((t) => r.selectorText.includes(t))) bad.push(r.selectorText);
        }
      }
    };
    for (const sheet of document.styleSheets) {
      try { walk(sheet.cssRules, false); } catch { /* foreign origin */ }
    }
    return bad;
  });
  check('no ungated :hover rules on composer buttons',
        strayHovers.length === 0, strayHovers.join(' | ') || 'none');

  // Smoke: the tap really does send, and the button stays accented.
  await page.tap('.input-area .send-btn');
  await new Promise(r => setTimeout(r, 400));
  await page.fill('#msg-textarea', 'again');
  await page.dispatchEvent('#msg-textarea', 'input');
  await new Promise(r => setTimeout(r, 150));
  const after = await css('.input-area .send-btn', 'background-color');
  check('send button stays accent-coloured after a tap',
        after !== 'rgba(0, 0, 0, 0)' && /^rgb/.test(after), 'bg=' + after);
  const sent = await page.evaluate(() =>
    [...document.querySelectorAll('.msg-row.out .msg-text')].some(n => n.textContent.trim() === 'hello'));
  check('control: the tap actually sent the message', sent === true);

  // ── 4. A tap on </> does not touch the field focus ────────────────────────
  // Exactly one thing keeps the keyboard open on a phone: the field not losing
  // focus. Playwright does not show a real keyboard, so we check what it depends
  // on — activeElement.
  await page.evaluate(() => document.getElementById('msg-textarea').focus());
  await new Promise(r => setTimeout(r, 100));
  await page.tap('#fmt-code-btn');
  await new Promise(r => setTimeout(r, 250));
  const focusAfter = await page.evaluate(() => document.activeElement && document.activeElement.id);
  check('touch: </> keeps focus in the field (the keyboard does not close)',
        focusAfter === 'msg-textarea', 'activeElement=' + focusAfter);
  const codeOn = await page.evaluate(() =>
    document.getElementById('fmt-code-btn').classList.contains('active'));
  check('control: </> still toggles code mode', codeOn === true);

  // Control: if the field was NOT focused, a tap on </> does not drag it there —
  // the keyboard must not pop up by itself.
  await page.evaluate(() => document.getElementById('msg-textarea').blur());
  await new Promise(r => setTimeout(r, 100));
  await page.tap('#fmt-code-btn');
  await new Promise(r => setTimeout(r, 200));
  const focusIdle = await page.evaluate(() => document.activeElement && document.activeElement.id);
  check('control: </> does not raise the keyboard from an unfocused field',
        focusIdle !== 'msg-textarea', 'activeElement=' + focusIdle);

  // ── 5. Enter on a phone is a line break, not a send ──────────────────────
  await page.evaluate(() => {
    const ta = document.getElementById('msg-textarea');
    ta.value = 'first'; ta.focus();
    ta.setSelectionRange(ta.value.length, ta.value.length);
    ta.dispatchEvent(new Event('input', { bubbles: true }));
  });
  const outBefore = await page.evaluate(() => document.querySelectorAll('.msg-row.out').length);
  await page.keyboard.press('Enter');
  await page.keyboard.type('second');
  await new Promise(r => setTimeout(r, 300));
  const afterEnter = await page.evaluate(() => ({
    value: document.getElementById('msg-textarea').value,
    out: document.querySelectorAll('.msg-row.out').length,
  }));
  check('mobile: Enter inserts a newline instead of sending',
        afterEnter.value === 'first\nsecond', JSON.stringify(afterEnter.value));
  check('control: nothing was sent by that Enter',
        afterEnter.out === outBefore, outBefore + ' → ' + afterEnter.out);
  await page.evaluate(() => {
    const ta = document.getElementById('msg-textarea');
    ta.value = ''; ta.dispatchEvent(new Event('input', { bubbles: true }));
  });

  // ── Desktop control: focus IS returned there (otherwise the fix broke the UX) ─
  const dctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const dpage = await dctx.newPage();
  dpage.on('pageerror', (e) => { console.log('  JS ERROR (desktop):', e.message); failures++; });
  await dpage.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await dpage.fill('#auth-username', alice);
  await dpage.fill('#auth-password', 'Password123!');
  await dpage.click('#auth-btn');
  await dpage.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
  await dpage.waitForSelector(`#contact-${bob}`, { timeout: 8000 });
  await dpage.click(`#contact-${bob}`);
  await dpage.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 300));
  await dpage.evaluate(() => document.getElementById('msg-textarea').blur());
  await dpage.click('#fmt-code-btn');
  await new Promise(r => setTimeout(r, 200));
  const dFocus = await dpage.evaluate(() => document.activeElement && document.activeElement.id);
  check('desktop control: </> DOES return focus to textarea',
        dFocus === 'msg-textarea', 'activeElement=' + dFocus);
  // On desktop the send button is always visible — there is no reason to hide it there.
  const dSend = await dpage.evaluate(() =>
    getComputedStyle(document.querySelector('.input-area .send-btn')).display !== 'none');
  check('desktop control: send button always visible', dSend === true);
  // Enter on desktop still SENDS — otherwise the change broke the main way to
  // send where there is a physical keyboard.
  const dOutBefore = await dpage.evaluate(() => document.querySelectorAll('.msg-row.out').length);
  await dpage.click('#msg-textarea');
  await dpage.type('#msg-textarea', 'desktop enter');
  await dpage.keyboard.press('Enter');
  await new Promise(r => setTimeout(r, 600));
  const dAfter = await dpage.evaluate(() => ({
    value: document.getElementById('msg-textarea').value,
    out: document.querySelectorAll('.msg-row.out').length,
  }));
  check('desktop control: Enter still sends', dAfter.out > dOutBefore && dAfter.value === '',
        JSON.stringify(dAfter));
  // Shift+Enter on desktop remains a line break.
  await dpage.type('#msg-textarea', 'line1');
  await dpage.keyboard.down('Shift');
  await dpage.keyboard.press('Enter');
  await dpage.keyboard.up('Shift');
  await dpage.type('#msg-textarea', 'line2');
  const dShift = await dpage.evaluate(() => document.getElementById('msg-textarea').value);
  check('desktop control: Shift+Enter still makes a newline',
        dShift === 'line1\nline2', JSON.stringify(dShift));

  // And the glass on desktop stays.
  const dbf = await dpage.evaluate(() =>
    getComputedStyle(document.querySelector('.input-area')).backdropFilter ||
    getComputedStyle(document.querySelector('.input-area')).webkitBackdropFilter);
  check('desktop control: glass preserved on .input-area',
        !!dbf && dbf !== 'none', 'backdrop-filter=' + dbf);

  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(2); });
