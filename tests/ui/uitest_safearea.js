// The transparent strip under the composer (the safe area on a phone).
//
// The real safe area comes from the OS and is zero in headless — so we substitute
// the height through --safe-bottom and measure the layout that follows from it.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
async function reg(u){const r=await fetch(BASE+'/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:'Password123!'})});if(!r.ok)throw new Error('reg '+r.status);return (await r.json()).token;}

(async () => {
  const a = U('sa_a'), b = U('sa_b'); await reg(a); const bt = await reg(b);
  const br = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const p = await (await br.newContext({ ...devices['Pixel 7'] })).newPage();
  p.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  await p.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await p.fill('#auth-username', a); await p.fill('#auth-password', 'Password123!'); await p.click('#auth-btn');
  await p.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bt}`);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  for (let i = 0; i < 30; i++) ws.send(JSON.stringify({ type:'message', id:'s'+i+Date.now(), from:b, to:a, body:'line '+i }));
  await new Promise(r => setTimeout(r, 900)); ws.close();
  await p.waitForSelector(`#contact-${b}`, { timeout: 8000 });
  await p.click(`#contact-${b}`);
  await p.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 500));

  // ── A device WITHOUT a cut-out: the strip is zero, nothing changes ────────
  const zero = await p.evaluate(() => {
    const cb = document.getElementById('chat-bottom');
    return { after: getComputedStyle(cb, '::after').height,
             areaBottom: Math.round(document.querySelector('.input-area').getBoundingClientRect().bottom),
             viewH: Math.round(window.innerHeight) };
  });
  check('no-notch device: strip is zero-height', parseFloat(zero.after) === 0, zero.after);
  check('no-notch device: composer still sits on the screen edge',
        Math.abs(zero.areaBottom - zero.viewH) <= 1, zero.areaBottom + ' vs ' + zero.viewH);

  // ── A device WITH A LARGE cut-out: Android with three buttons, ~48px ──────
  // The reserve is capped at 12px — otherwise a finger-tall strip of emptiness is
  // left at the bottom, and there are never any messages in it anyway.
  await p.evaluate(() => {
    document.getElementById('chat-bottom').style.setProperty('--safe-raw', '48px');
  });
  await new Promise(r => setTimeout(r, 400));
  const notch = await p.evaluate(() => {
    const cb = document.getElementById('chat-bottom');
    const area = document.querySelector('.input-area');
    const wrap = document.getElementById('messages-wrap');
    const host = wrap.parentElement;
    return {
      after: getComputedStyle(cb, '::after').height,
      areaBottom: Math.round(area.getBoundingClientRect().bottom),
      viewH: Math.round(window.innerHeight),
      padBottom: getComputedStyle(host).getPropertyValue('--feed-pad-bottom').trim(),
      cbHeight: Math.round(cb.getBoundingClientRect().height),
      cbBg: getComputedStyle(cb).backgroundColor,
      areaBg: getComputedStyle(area).backgroundColor,
      taBg: getComputedStyle(document.getElementById('msg-textarea')).backgroundColor,
    };
  });
  check('48px inset is capped at 12px', parseFloat(notch.after) === 12, notch.after);
  check('composer is lifted by exactly the capped strip',
        notch.viewH - notch.areaBottom === 12, 'gap=' + (notch.viewH - notch.areaBottom) + 'px');
  check('the strip itself is transparent',
        notch.cbBg === 'rgba(0, 0, 0, 0)', 'chat-bottom bg=' + notch.cbBg);
  // The panel is deliberately transparent now — the control is the input field: if
  // it became transparent too there would be nothing to type on.
  check('control: the input field keeps its own opaque background',
        notch.taBg !== 'rgba(0, 0, 0, 0)', 'textarea bg=' + notch.taBg);
  check('feed padding accounts for the strip',
        parseFloat(notch.padBottom) === notch.cbHeight,
        '--feed-pad-bottom=' + notch.padBottom + ' vs #chat-bottom=' + notch.cbHeight + 'px');

  // A cut-out SMALLER than the ceiling is used as is — the ceiling must not inflate it.
  await p.evaluate(() => {
    document.getElementById('chat-bottom').style.setProperty('--safe-raw', '6px');
  });
  await new Promise(r => setTimeout(r, 300));
  const small = await p.evaluate(() =>
    getComputedStyle(document.getElementById('chat-bottom'), '::after').height);
  check('control: a small inset (6px) passes through unchanged',
        parseFloat(small) === 6, small);
  await p.evaluate(() => {
    document.getElementById('chat-bottom').style.setProperty('--safe-raw', '48px');
  });
  await new Promise(r => setTimeout(r, 300));

  // The last message at rest does NOT slide under the composer.
  await p.evaluate(() => { const w = document.getElementById('messages-wrap'); w.scrollTop = w.scrollHeight; });
  await new Promise(r => setTimeout(r, 400));
  const last = await p.evaluate(() => {
    const rows = document.querySelectorAll('.msg-row');
    const r = rows[rows.length - 1].getBoundingClientRect();
    const area = document.querySelector('.input-area').getBoundingClientRect();
    return Math.round(area.top - r.bottom);
  });
  check('notch device: last message stays clear of the composer', last >= 0, 'clearance=' + last + 'px');

  await br.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
