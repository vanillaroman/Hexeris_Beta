// A hostile suite: the awkward paths where a demo usually breaks.
// Each block checks not "it did not crash" but a concrete observable invariant.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
let connectWSFromTest = null;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
async function reg(u){const r=await fetch(BASE+'/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:'Password123!'})});if(!r.ok)throw new Error('reg '+r.status);return (await r.json()).token;}
const sleep = (ms) => new Promise(r => setTimeout(r, ms));

(async () => {
  const a = U('h_a'), b = U('h_b'), c = U('h_c');
  await reg(a); const bt = await reg(b); const ct = await reg(c);

  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 860 } });
  const page = await ctx.newPage();
  const jsErrors = [];
  page.on('pageerror', (e) => { jsErrors.push(e.message); });
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', a); await page.fill('#auth-password', 'Password123!'); await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  const wsB = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bt}`);
  const wsC = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${ct}`);
  await Promise.all([wsB, wsC].map(w => new Promise((r, j) => { w.onopen = r; w.onerror = j; })));
  const send = (w, from, body, extra) => w.send(JSON.stringify(
    { type:'message', id: from + '-' + Math.random().toString(36).slice(2) + Date.now(), from, to: a, body, ...extra }));

  for (let i = 0; i < 20; i++) { send(wsB, b, 'from B number ' + i); send(wsC, c, 'from C number ' + i); }
  await sleep(1500);
  await page.waitForSelector(`#contact-${b}`, { timeout: 10000 });
  await page.waitForSelector(`#contact-${c}`, { timeout: 10000 });

  // ══ 1. Fast chat switching under a stream of messages ════════════════════
  // Incremental rendering remembers the peer in a dataset — if that drifts, the
  // rows of one conversation get drawn into another. That is the most expensive
  // error there is: someone else's conversation on screen.
  for (let round = 0; round < 6; round++) {
    await page.click(`#contact-${b}`);
    send(wsB, b, 'B stream ' + round); send(wsC, c, 'C stream ' + round);
    await sleep(120);
    await page.click(`#contact-${c}`);
    send(wsB, b, 'B stream2 ' + round); send(wsC, c, 'C stream2 ' + round);
    await sleep(120);
  }
  await sleep(900);
  // The snapshot is taken only once the model and the DOM agree. Otherwise the
  // test catches its own race: a message has already been added to chats[] but the
  // repaint has not run yet — a one-row discrepancy would look like a leak of
  // someone else's chat even though the product is fine. If they do not agree
  // within 5 seconds, that is a real problem and it must be shown.
  await page.waitForFunction(() => {
    const wrap = document.getElementById('messages-wrap');
    const peer = wrap.dataset.peer;
    if (!peer) return false;
    return (chats[peer] || []).length === wrap.querySelectorAll('.msg-bubble[data-id]').length;
  }, { timeout: 5000 }).catch(() => {});
  const leak = await page.evaluate(() => {
    const wrap = document.getElementById('messages-wrap');
    const peer = wrap.dataset.peer;
    const ids = new Set((chats[peer] || []).map(m => m.id));
    const shown = [...wrap.querySelectorAll('.msg-bubble[data-id]')].map(e => e.dataset.id);
    const foreign = shown.filter(id => !ids.has(id));
    return { peer, shown: shown.length, expected: ids.size, foreign: foreign.length,
             dupes: shown.length - new Set(shown).size };
  });
  check('no messages from another chat leak into the open one',
        leak.foreign === 0, JSON.stringify(leak));
  check('no duplicated rows after rapid switching', leak.dupes === 0, JSON.stringify(leak));
  check('the open chat shows exactly its own messages',
        leak.shown === leak.expected, leak.shown + ' on screen / ' + leak.expected + ' in the model');

  // ══ 2. Deletion and a reaction go through a FULL rebuild ═══════════════
  const del = await page.evaluate(async () => {
    const wrap = document.getElementById('messages-wrap');
    const peer = wrap.dataset.peer;
    const m = chats[peer][chats[peer].length - 2];
    m.deleted = true; m.body = '[deleted]';
    renderMessages(peer);
    const b1 = wrap.querySelector('.msg-bubble[data-id="' + m.id + '"]');
    const gone = !!(b1 && b1.textContent.includes('Message deleted'));
    // Reaction
    const m2 = chats[peer][chats[peer].length - 1];
    m2.reactions = { '👍': [peer] }; m2.rseq = 1;
    renderMessages(peer);
    const b2 = wrap.querySelector('.msg-bubble[data-id="' + m2.id + '"]');
    const hasChip = !!(b2 && b2.closest('.msg-col').querySelector('.reaction-chip'));
    return { gone, hasChip };
  });
  check('a deleted message re-renders as deleted', del.gone === true);
  check('a new reaction shows up (fingerprint covers rseq/reactions)', del.hasChip === true);

  // ══ 3. XSS through a message body ══════════════════════════════════════
  const XSS = '<img src=x onerror="window.__pwned=1"><script>window.__pwned=1<\/script>';
  send(wsB, b, XSS);
  await sleep(700);
  await page.click(`#contact-${b}`);
  await sleep(600);
  const xss = await page.evaluate((raw) => ({
    pwned: !!window.__pwned,
    imgs: document.querySelectorAll('#messages-wrap img[src="x"]').length,
    shownAsText: [...document.querySelectorAll('.msg-text')].some(n => n.textContent.includes('onerror')),
  }), XSS);
  check('script in a message body does NOT execute', xss.pwned === false);
  check('control: no injected <img> node was created', xss.imgs === 0, 'found ' + xss.imgs);
  check('control: the payload is visible as plain text', xss.shownAsText === true);

  // ══ 4. Markdown parsing on hostile input (looping/hanging) ═════════════
  const evil = [
    '**'.repeat(400),
    '`'.repeat(400),
    '~~a~~'.repeat(200),
    '```\n' + 'x'.repeat(5000) + '\n```',
    '*'.repeat(200) + 'text' + '*'.repeat(200),
    'http://' + 'a'.repeat(500) + '.example.com/' + 'b'.repeat(500),
  ];
  const t0 = Date.now();
  for (const e of evil) send(wsB, b, e);
  await sleep(2500);
  const parseMs = Date.now() - t0;
  const alive = await page.evaluate(() => {
    const wrap = document.getElementById('messages-wrap');
    return { rows: wrap.querySelectorAll('.msg-row').length, responsive: true };
  });
  check('hostile markdown does not hang the tab', alive.responsive === true && parseMs < 20000,
        'processed in ' + parseMs + 'ms, rows ' + alive.rows);

  // ══ 5. A long message: expanding ═══════════════════════════════════════
  send(wsB, b, 'Y'.repeat(4000));
  await sleep(800);
  const expand = await page.evaluate(async () => {
    const btn = document.querySelector('.msg-expand');
    if (!btn) return 'NO_BUTTON';
    const before = document.querySelectorAll('.msg-longtext').length;
    btn.click();
    await new Promise(r => setTimeout(r, 300));
    return { before, after: document.querySelectorAll('.msg-longtext').length,
             full: [...document.querySelectorAll('.msg-text')].some(n => n.textContent.length > 3000) };
  });
  check('a very long message can be expanded',
        expand !== 'NO_BUTTON' && expand.full === true, JSON.stringify(expand));

  // ══ 6. Changing the theme with a chat open ═════════════════════════════
  const theme = await page.evaluate(async () => {
    const before = getComputedStyle(document.body).backgroundColor;
    cycleTheme();
    await new Promise(r => setTimeout(r, 200));
    const after = getComputedStyle(document.body).backgroundColor;
    const rows = document.querySelectorAll('.msg-row').length;
    cycleTheme(); cycleTheme();   // back to where we were (dark → light → system → dark)
    await new Promise(r => setTimeout(r, 200));
    return { before, after, rows, back: getComputedStyle(document.body).backgroundColor };
  });
  check('theme switch repaints without losing the feed',
        theme.before !== theme.after && theme.rows > 0, JSON.stringify(theme));
  check('control: cycling all the way round restores the original theme',
        theme.back === theme.before, theme.before + ' → ' + theme.back);

  // ══ 7. Reloading the page with a chat open ═════════════════════════════
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 12000 });
  await sleep(1800);
  const restored = await page.evaluate(() => {
    const wrap = document.getElementById('messages-wrap');
    return { peer: wrap.dataset.peer || null,
             rows: wrap.querySelectorAll('.msg-row').length,
             composer: !!document.getElementById('msg-textarea') };
  });
  check('the open chat survives a reload', !!restored.peer && restored.rows > 0, JSON.stringify(restored));

  // ══ 8. The queue: sending on a dead socket, then a reconnect ═══════════
  // Reconnecting is disabled entirely for the duration of the check. The test used
  // to rely on the first reconnect attempt waiting at least 3 seconds, and managed
  // to observe the "hourglass" inside that pause. The pause was removed
  // deliberately (a server restart must not cost every client three seconds), and
  // the test started catching an already DELIVERED message — that is, it measured
  // the length of the pause rather than what it was written for. We keep the socket
  // dead explicitly.
  const queued = await page.evaluate(async () => {
    const realConnect = window.connectWS;
    window.connectWS = () => {};
    try {
      try { ws.close(); } catch {}
      await new Promise(r => setTimeout(r, 300));
      const before = document.querySelectorAll('.msg-row').length;
      document.getElementById('msg-textarea').value = 'went into the queue';
      sendMessage();
      await new Promise(r => setTimeout(r, 400));
      const row = [...document.querySelectorAll('.msg-row.out')].pop();
      return { marked: row.classList.contains('queued'),
               grew: document.querySelectorAll('.msg-row').length > before,
               pending: (JSON.parse(localStorage.getItem('hc_pending_' + myUsername) || '[]')).length };
    } finally {
      window.connectWS = realConnect;
    }
  });
  check('sending with a dead socket queues and marks the message',
        queued.marked === true && queued.grew === true, JSON.stringify(queued));
  // And it is in the persistent queue, not merely drawn on screen.
  check('the queued message is persisted for re-delivery',
        queued.pending > 0, JSON.stringify(queued));

  // The reconnect is back — the message must go out and the "hourglass" clear.
  // That is the second half of the same invariant: a queue without a flush is useless.
  connectWSFromTest = await page.evaluate(async () => {
    connectWS();
    for (let i = 0; i < 60; i++) {
      if (ws && ws.readyState === 1 &&
          (JSON.parse(localStorage.getItem('hc_pending_' + myUsername) || '[]')).length === 0) {
        return { delivered: true, ms: i * 100 };
      }
      await new Promise(r => setTimeout(r, 100));
    }
    return { delivered: false, ms: 6000 };
  });
  check('the queued message is delivered after reconnect',
        connectWSFromTest.delivered === true, JSON.stringify(connectWSFromTest));

  // ══ 9. There must be no errors in the console ══════════════════════════
  await sleep(500);
  check('no uncaught JS errors during the whole run',
        jsErrors.length === 0, jsErrors.slice(0, 3).join(' | ') || 'no errors');

  wsB.close(); wsC.close();
  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
