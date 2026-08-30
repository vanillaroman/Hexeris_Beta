// Four items: incremental rendering, grouping, Ctrl+K and the "queued" state.
// Plus a fully transparent input panel.
const { chromium, devices } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
const U = (p) => p + Math.floor(Math.random() * 1e9);
let failures = 0;
const check = (n, ok, x) => { console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : '')); if (!ok) failures++; };
async function reg(u){const r=await fetch(BASE+'/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:u,password:'Password123!'})});if(!r.ok)throw new Error('reg '+r.status);return (await r.json()).token;}

(async () => {
  const a = U('pf_a'), b = U('pf_b'); await reg(a); const bt = await reg(b);
  const browser = await chromium.launch({ ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });

  // ═══ Desktop: Ctrl+K and performance ═══
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 800 } })).newPage();
  page.on('pageerror', (e) => { console.log('  JS ERROR:', e.message); failures++; });
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.fill('#auth-username', a); await page.fill('#auth-password', 'Password123!'); await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });

  const ws = new WebSocket(`${BASE.replace(/^http/, 'ws')}/ws?token=${bt}`);
  await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
  // 400 messages — the size at which a full rebuild is already noticeable.
  for (let i = 0; i < 400; i++) {
    ws.send(JSON.stringify({ type:'message', id:'p'+i+'-'+Date.now(), from:b, to:a, body:'Message number '+i }));
  }
  await new Promise(r => setTimeout(r, 3000));
  await page.waitForSelector(`#contact-${b}`, { timeout: 10000 });
  await page.click(`#contact-${b}`);
  await page.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 1200));

  const rows = await page.evaluate(() => document.querySelectorAll('.msg-row').length);
  check('control: the feed really is large', rows >= 300, rows + ' rows');

  // ── 1. Incremental rendering ─────────────────────────────────────────────
  // We measure not "is it fast" but HOW MANY NODES are created: timings are noisy
  // in headless, while the number of recreated rows follows directly from the fix.
  const perf = await page.evaluate(() => {
    const wrap = document.getElementById('messages-wrap');
    const peer = wrap.dataset.peer;
    // takeRecords(), not a callback: the MutationObserver callback runs in a
    // microtask, and a disconnect() right after renderMessages would throw away
    // what had accumulated — the counter would always show zero, that is, the
    // check would pass on a full rebuild too.
    const obs = new MutationObserver(() => {});
    obs.observe(wrap, { childList: true });
    // One new message — as it arrives over the socket.
    chats[peer].push({ id: 'perf-' + Date.now(), from: peer, to: myUsername,
                       body: 'one more', ts: Date.now(), status: 'sent' });
    const t0 = performance.now();
    renderMessages(peer);
    const ms = performance.now() - t0;
    const rebuilt = obs.takeRecords().reduce((n, r) => n + r.addedNodes.length, 0);
    obs.disconnect();
    return { rebuilt, ms: Math.round(ms), total: wrap.querySelectorAll('.msg-row').length };
  });
  check('one new message adds one row instead of rebuilding the feed',
        perf.rebuilt <= 2, 'nodes created: ' + perf.rebuilt + ' for ' + perf.total + ' rows');
  check('control: the message actually appeared', perf.total >= rows + 1,
        rows + ' → ' + perf.total);

  // Editing an existing message must lead to a FULL rebuild — otherwise the
  // changed text simply never appears on screen.
  const edit = await page.evaluate(() => {
    const wrap = document.getElementById('messages-wrap');
    const peer = wrap.dataset.peer;
    const obs = new MutationObserver(() => {});
    obs.observe(wrap, { childList: true });
    chats[peer][5].body = 'CHANGED TEXT';
    chats[peer][5].edited = true;
    renderMessages(peer);
    const added = obs.takeRecords().reduce((n, r) => n + r.addedNodes.length, 0);
    obs.disconnect();
    const shown = [...wrap.querySelectorAll('.msg-text')].some(n => n.textContent.includes('CHANGED TEXT'));
    return { added, shown };
  });
  check('negative control: an edit still triggers a full rebuild',
        edit.added > 50, 'nodes created: ' + edit.added);
  check('negative control: the edited text is actually on screen', edit.shown === true);

  // A repeat call with no changes must not touch the DOM at all.
  const noop = await page.evaluate(() => {
    const wrap = document.getElementById('messages-wrap');
    const obs = new MutationObserver(() => {});
    obs.observe(wrap, { childList: true });
    renderMessages(wrap.dataset.peer);
    const added = obs.takeRecords().reduce((n, r) => n + r.addedNodes.length, 0);
    obs.disconnect();
    return added;
  });
  check('an unchanged re-render touches nothing', noop === 0, 'nodes created: ' + noop);

  // ── 2. Grouping ──────────────────────────────────────────────────────────
  const grouped = await page.evaluate(() => {
    const all = [...document.querySelectorAll('.msg-row')];
    return { total: all.length, grouped: all.filter(r => r.classList.contains('grouped')).length,
             firstGrouped: all.length ? all[0].classList.contains('grouped') : null };
  });
  check('consecutive messages from one sender are grouped',
        grouped.grouped > grouped.total * 0.5, grouped.grouped + ' of ' + grouped.total);
  check('control: the first row is never grouped', grouped.firstGrouped === false);

  // Changing the sender breaks the group.
  const breaks = await page.evaluate(() => {
    const wrap = document.getElementById('messages-wrap');
    const peer = wrap.dataset.peer;
    chats[peer].push({ id: 'mine-' + Date.now(), from: myUsername, to: peer,
                       body: 'my reply', ts: Date.now(), status: 'sent' });
    renderMessages(peer);
    const all = [...document.querySelectorAll('.msg-row')];
    return all[all.length - 1].classList.contains('grouped');
  });
  check('control: a different sender breaks the group', breaks === false);

  // ── 3. Ctrl/Cmd+K ────────────────────────────────────────────────────────
  await page.evaluate(() => document.getElementById('msg-textarea').focus());
  await page.keyboard.press('Control+k');
  await new Promise(r => setTimeout(r, 250));
  const focused = await page.evaluate(() => document.activeElement && document.activeElement.id);
  check('Ctrl+K focuses the global search', focused === 'search-input', 'activeElement=' + focused);

  await page.fill('#search-input', 'Message number 1');
  await page.waitForSelector('#search-results .search-hit', { timeout: 10000 });
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('ArrowDown');
  await new Promise(r => setTimeout(r, 200));
  const cursor = await page.evaluate(() => {
    const hits = [...document.querySelectorAll('#search-results .search-hit')];
    return { n: hits.length, active: hits.findIndex(h => h.classList.contains('sr-active')) };
  });
  check('arrow keys move a visible cursor through results',
        cursor.active === 1, 'highlighted #' + cursor.active + ' of ' + cursor.n);
  await page.keyboard.press('ArrowUp');
  await new Promise(r => setTimeout(r, 150));
  const up = await page.evaluate(() =>
    [...document.querySelectorAll('#search-results .search-hit')].findIndex(h => h.classList.contains('sr-active')));
  check('control: ArrowUp moves back', up === 0, '#' + up);

  await page.keyboard.press('Escape');
  await new Promise(r => setTimeout(r, 250));
  const cleared = await page.evaluate(() => document.getElementById('search-input').value);
  check('Esc clears the query first', cleared === '', JSON.stringify(cleared));

  // ── 4. The "queued" state ────────────────────────────────────────────────
  const queued = await page.evaluate(() => {
    // Break the socket — the message goes into the persistent outbox, not the network.
    try { ws.close(); } catch {}
    const peer = document.getElementById('messages-wrap').dataset.peer;
    const id = 'q-' + Date.now();
    queueMessage({ type: 'message', id, from: myUsername, to: peer, body: 'waiting for the network' });
    addToChat(peer, { id, from: myUsername, to: peer, body: 'waiting for the network', status: 'sending', ts: Date.now() });
    renderMessages(peer);
    const row = document.querySelector('.msg-bubble[data-id="' + id + '"]');
    const r = row && row.closest('.msg-row');
    return { marked: !!(r && r.classList.contains('queued')),
             isQueued: typeof isQueued === 'function' && isQueued(id),
             opacity: r ? getComputedStyle(r.querySelector('.msg-bubble')).opacity : null };
  });
  check('a message waiting for the network is marked as queued', queued.marked === true);
  check('control: the outbox really holds it', queued.isQueued === true);
  check('queued bubble is visually dimmed', parseFloat(queued.opacity) < 1, 'opacity=' + queued.opacity);

  const notQueued = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.msg-row')];
    return rows.filter(r => r.classList.contains('queued')).length;
  });
  check('control: only the waiting message is marked, not the whole feed',
        notQueued === 1, 'rows marked: ' + notQueued);

  // ── The input panel is fully transparent (phone) ─────────────────────────
  const mob = await (await browser.newContext({ ...devices['Pixel 7'] })).newPage();
  mob.on('pageerror', (e) => { console.log('  JS ERROR (mobile):', e.message); failures++; });
  await mob.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await mob.fill('#auth-username', a); await mob.fill('#auth-password', 'Password123!'); await mob.click('#auth-btn');
  await mob.waitForSelector('#chat-screen', { state: 'visible', timeout: 10000 });
  await mob.waitForSelector(`#contact-${b}`, { timeout: 10000 });
  await mob.click(`#contact-${b}`);
  await mob.waitForSelector('#msg-textarea', { state: 'visible', timeout: 8000 });
  await new Promise(r => setTimeout(r, 600));
  const panel = await mob.evaluate(() => {
    const area = document.querySelector('.input-area');
    const cs = getComputedStyle(area);
    const ta = getComputedStyle(document.getElementById('msg-textarea'));
    return { bg: cs.backgroundColor, bf: cs.backdropFilter || cs.webkitBackdropFilter,
             border: cs.borderTopWidth, mask: getComputedStyle(document.getElementById('chat-bottom'), '::before').display,
             taBg: ta.backgroundColor };
  });
  check('mobile composer panel is fully transparent', panel.bg === 'rgba(0, 0, 0, 0)', 'bg=' + panel.bg);
  check('mobile composer has no blur left', !panel.bf || panel.bf === 'none', 'backdrop-filter=' + panel.bf);
  check('mobile composer has no divider line', parseFloat(panel.border) === 0, 'border=' + panel.border);
  check('the gradient mask is gone', panel.mask === 'none', 'display=' + panel.mask);
  check('control: the input field keeps its own opaque background',
        !/rgba\([^)]*,\s*0?\.\d+\s*\)/.test(panel.taBg) && panel.taBg !== 'rgba(0, 0, 0, 0)', 'textarea bg=' + panel.taBg);

  await browser.close();
  console.log(failures ? `\n${failures} FAILED` : '\nALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
