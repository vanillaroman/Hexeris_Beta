// Markup, the theme, the "send as code" button and interface response.
//
// The main check here is XSS: the markup is written by a user, and if the
// renderer assembles an HTML string anywhere, an inserted tag becomes a node. We
// check not "the function did not crash" but that the DOM does NOT contain an
// element that should not be there.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');

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

async function say(tok, from, to, body) {
  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${tok}`);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  ws.send(JSON.stringify({ type: 'message', id: String(Date.now()) + Math.random(), from, to, body }));
  await new Promise(r => setTimeout(r, 400));
  ws.close();
}

(async () => {
  const me = U('ui_me'), peer = U('ui_peer');
  await register(me);
  const peerTok = await register(peer);

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });

  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', me);
  await page.fill('#auth-password', 'Password123!');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  // ── XSS: the reason the renderer builds a DOM rather than a string ──
  const attacks = [
    '<img src=x onerror="window.__pwned=1">',
    '<script>window.__pwned=1</script>',
    '**<img src=x onerror="window.__pwned=1">**',
    '`<img src=x onerror="window.__pwned=1">`',
    '```\n<img src=x onerror="window.__pwned=1">\n```',
    '> <img src=x onerror="window.__pwned=1">',
    '[x](javascript:window.__pwned=1)',
  ];
  for (const a of attacks) await say(peerTok, peer, me, a);
  await page.waitForSelector(`#contact-${peer}`, { timeout: 8000 });
  // A pause before the click: the list rows are recreated on every incoming
  // message, and Playwright will retry a click on a node that is about to be
  // replaced until it times out. That is a property of the test rather than of the
  // interface — in the check above the messages arrive as a batch within a second,
  // which never happens with a person.
  await page.waitForTimeout(1500);
  await page.click(`#contact-${peer}`);
  await page.waitForSelector('.msg-bubble', { timeout: 5000 });
  await page.waitForTimeout(600);

  const pwned = await page.evaluate(() => !!window.__pwned);
  check('XSS: the handler did not run', !pwned);
  const injected = await page.evaluate(() =>
    document.querySelectorAll('.msg-bubble img, .msg-bubble script').length);
  check('XSS: no injected nodes in the feed', injected === 0, 'found: ' + injected);
  const shownRaw = await page.evaluate(() =>
    [...document.querySelectorAll('.msg-text')].some(e => e.textContent.includes('<img src=x')));
  check('control: the attack text is shown as text', shownRaw);

  // ── Markup ──
  await say(peerTok, peer, me, 'ordinary **bold** and *italic* and ~~strikethrough~~ and `code`');
  await page.waitForTimeout(700);
  const marks = await page.evaluate(() => ({
    strong: document.querySelectorAll('.msg-bubble strong').length,
    em:     document.querySelectorAll('.msg-bubble em').length,
    s:      document.querySelectorAll('.msg-bubble s').length,
    code:   document.querySelectorAll('.msg-bubble .md-inline-code').length,
  }));
  check('bold/italic/strikethrough/code are parsed',
    marks.strong >= 1 && marks.em >= 1 && marks.s >= 1 && marks.code >= 1, JSON.stringify(marks));

  // snake_case must not turn into italics — that is why _italic_ was dropped.
  await say(peerTok, peer, me, 'export MY_VAR_NAME=1 and 5 * 3 = 15 * 1');
  await page.waitForTimeout(700);
  const falsePositives = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.msg-bubble')];
    const last = rows[rows.length - 1];
    return { em: last.querySelectorAll('em').length, text: last.textContent.trim() };
  });
  check('snake_case and multiplication did not become markup',
    falsePositives.em === 0, JSON.stringify(falsePositives));

  // ── Line breaks ──
  await say(peerTok, peer, me, 'first line\nsecond line\n  indented');
  await page.waitForTimeout(700);
  const nl = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.msg-bubble')];
    const last = rows[rows.length - 1];
    return { ws: getComputedStyle(last).whiteSpace, height: last.getBoundingClientRect().height };
  });
  check('line breaks are preserved', nl.ws === 'pre-wrap' && nl.height > 40, JSON.stringify(nl));

  // ── A code block with a Copy button ──
  await say(peerTok, peer, me, '```bash\nsudo systemctl status docker\nsudo docker ps -a\n```');
  await page.waitForTimeout(700);
  check('the code block is rendered', await page.isVisible('.md-codebox'));
  check('the block has a Copy button', await page.isVisible('.md-copy'));
  const lang = await page.evaluate(() => {
    const l = [...document.querySelectorAll('.md-codelang')];
    return l[l.length - 1].textContent;
  });
  check('the block language is labelled', lang === 'bash', lang);

  // ── The </> button in the composer ──
  await page.click('#fmt-code-btn');
  check('code mode turned on', await page.evaluate(() =>
    document.getElementById('fmt-code-btn').classList.contains('active')));
  await page.fill('#msg-textarea', 'echo hello');
  await page.click('.send-btn');
  await page.waitForTimeout(900);
  const sentAsCode = await page.evaluate(() => {
    const boxes = [...document.querySelectorAll('.msg-row.out .md-codebox')];
    return boxes.length > 0 && boxes[boxes.length - 1].textContent.includes('echo hello');
  });
  check('the message went out as a code block', sentAsCode);
  check('the mode reset after sending', await page.evaluate(() =>
    !document.getElementById('fmt-code-btn').classList.contains('active')));

  // Control: without pressing the button the text goes out as usual.
  await page.fill('#msg-textarea', 'just text');
  await page.click('.send-btn');
  await page.waitForTimeout(800);
  const plain = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.msg-row.out')];
    const last = rows[rows.length - 1];
    return { code: last.querySelectorAll('.md-codebox').length, text: last.textContent };
  });
  check('control: ordinary text did not become code',
    plain.code === 0 && plain.text.includes('just text'), JSON.stringify(plain.code));

  // ── Bubble width ──
  await say(peerTok, peer, me, 'a long line '.repeat(40));
  await page.waitForTimeout(700);
  const w = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.msg-row.in .msg-bubble')];
    return rows[rows.length - 1].getBoundingClientRect().width;
  });
  check('the bubble is bounded by line length rather than a percentage of the window',
    w < 700, Math.round(w) + 'px in a 1280 window');

  // ── Theme ──
  // Control: by default the theme is DARK regardless of the browser's system
  // setting — introducing a toggle must not change the look for people who never
  // touched it. In headless prefers-color-scheme = light, so the check does not
  // degenerate.
  const darkBg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const lumOf = (c) => { const m = c.match(/\d+/g); return (+m[0] * .2126 + +m[1] * .7152 + +m[2] * .0722); };
  check('the default theme is dark rather than the system one', lumOf(darkBg) < 60, darkBg);
  await page.click('#settings-btn');
  await page.waitForTimeout(200);
  await page.click('#settings-theme');   // dark → light
  await page.waitForTimeout(400);
  const lightBg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  check('the light theme changes the background', lightBg !== darkBg, `${darkBg} → ${lightBg}`);
  check('the theme attribute is set', await page.evaluate(() =>
    document.documentElement.getAttribute('data-theme') === 'light'));
  check('color-scheme is handed to the browser', await page.evaluate(() =>
    getComputedStyle(document.documentElement).colorScheme === 'light'));
  // The text must stay readable: a light theme without recomputing --text would
  // mean white letters on white.
  const textCol = await page.evaluate(() => getComputedStyle(document.body).color);
  const lum = (c) => { const m = c.match(/\d+/g); return (+m[0] * .2126 + +m[1] * .7152 + +m[2] * .0722); };
  check('in the light theme the text is dark', lum(textCol) < 100, textCol);

  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForTimeout(1200);
  check('the theme survived a reload', await page.evaluate(() =>
    document.documentElement.getAttribute('data-theme') === 'light'));

  // ── Glass ──
  const glass = await page.evaluate(() => {
    const n = document.querySelectorAll('*');
    let count = 0;
    for (const el of n) {
      const f = getComputedStyle(el).backdropFilter || getComputedStyle(el).webkitBackdropFilter;
      if (f && f !== 'none') count++;
    }
    const h = getComputedStyle(document.querySelector('.chat-header'));
    return { count, headerFilter: h.backdropFilter || h.webkitBackdropFilter };
  });
  check('the glass on the header is raised to 28px',
    /28px/.test(glass.headerFilter), glass.headerFilter);

  await browser.close();
  console.log(failures ? `\nFAILED checks: ${failures}` : '\nAll checks passed');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('ERROR:', e); process.exit(2); });
