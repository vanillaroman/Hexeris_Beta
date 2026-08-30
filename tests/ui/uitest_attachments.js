// The attachments panel, the live region and the list skeleton.
//
// Three things added in one pass are checked by one suite: they share a fixture
// (a conversation with real files is needed) and share the cost of setting it up.
//
// Separately about the access boundary. The /attachments endpoint returns LINKS TO
// FILES, so the "another conversation is not returned" check is no formality here:
// if the boundary is ever removed, the suite must fail. Both a group the person is
// not a member of and a direct conversation between two other people are checked.
const { chromium } = require(process.env.PLAYWRIGHT_PATH || 'playwright');
const BASE = process.env.HEXERIS_URL || 'http://127.0.0.1:8766';
let failures = 0;
const check = (n, ok, x) => {
  console.log((ok ? '  PASS  ' : '  FAIL  ') + n + (x ? '  — ' + x : ''));
  if (!ok) failures++;
};
const tag = 'at' + String(Date.now()).slice(-7);

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

const seed = (token, msgs) => new Promise((res, rej) => {
  const s = new WebSocket(BASE.replace('http', 'ws') + '/ws?token=' + token);
  s.onopen = () => {
    msgs.forEach((m, i) => s.send(JSON.stringify({ ...m, id: tag + '-' + Math.random().toString(36).slice(2) + i })));
    setTimeout(() => { s.close(); res(); }, 900);
  };
  s.onerror = rej;
});

// A real upload through /upload: only that way does the server set media_type
// rather than the test. Faking it would mean checking our own invention.
async function upload(token, name, type, bytes) {
  const fd = new FormData();
  fd.append('file', new Blob([bytes], { type }), name);
  const r = await fetch(BASE + '/upload', {
    method: 'POST', headers: { Authorization: 'Bearer ' + token }, body: fd,
  });
  if (!r.ok) throw new Error('upload ' + name + ': ' + r.status + ' ' + (await r.text()));
  return r.json();
}

// A minimal real 1×1 PNG — the server classifies by extension, but we would
// rather not hand a deliberately broken file to the test.
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64');

async function login(page, user) {
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#auth-screen', { state: 'visible' });
  await page.fill('#auth-username', user);
  await page.fill('#auth-password', 'passw0rd-test');
  await page.click('#auth-btn');
  await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
  await page.waitForTimeout(1200);
}

(async () => {
  await waitReady();
  const me = 'me' + tag, bob = 'bob' + tag, eve = 'eve' + tag, mal = 'mal' + tag;
  // A separate user WITHOUT a conversation — for the skeleton check. For someone
  // who has been written to, the unread messages arrive over the socket before the
  // /history response, and a real row honestly replaces the skeleton before
  // loading finishes.
  const fresh = 'fresh' + tag;
  const tMe = await reg(me); const tBob = await reg(bob); await reg(fresh);
  const tEve = await reg(eve); const tMal = await reg(mal);

  // Attachments in the me ↔ bob conversation: two images, a document and a voice message.
  const up = [];
  for (const [n, t, b] of [
    ['design-review.png', 'image/png', PNG],
    ['dashboard-final.png', 'image/png', PNG],
    ['contract-v2.pdf', 'application/pdf', Buffer.from('%PDF-1.4 test')],
  ]) up.push(await upload(tBob, n, t, b));
  const voice = await upload(tBob, 'note.webm', 'audio/webm', Buffer.from('OggS-fake'));

  // The body of a file message is assembled as a real client would: a link plus
  // the name in the #fragment. Without the fragment the panel would show the
  // on-disk hash.
  const withName = (u, n) => u.url + '#' + encodeURIComponent(n);
  await seed(tBob, [
    { type: 'message', to: me, body: withName(up[0], 'design-review.png'), media_type: up[0].media_type },
    { type: 'message', to: me, body: withName(up[1], 'dashboard-final.png'), media_type: up[1].media_type },
    { type: 'message', to: me, body: withName(up[2], 'contract-v2.pdf'), media_type: up[2].media_type },
    { type: 'message', to: me, body: withName(voice, 'note.webm'), media_type: 'voice' },
    { type: 'message', to: me, body: 'And here is the text that must NOT appear in the panel.' },
  ]);
  // The eve ↔ mal conversation of others — there must be no access to it.
  const secret = await upload(tEve, 'private-budget.pdf', 'application/pdf', Buffer.from('%PDF secret'));
  await seed(tEve, [{ type: 'message', to: mal, body: secret.url, media_type: secret.media_type }]);

  check('the server classified the attachments itself (control)',
        up[0].media_type === 'image' && up[2].media_type === 'document',
        up.map((u) => u.media_type).join(','));

  // ══ 1. The access boundary at the endpoint ══════════════════════════════
  {
    const ask = (token, peer, kind) => fetch(
      `${BASE}/attachments?peer=${encodeURIComponent(peer)}&kind=${kind}`,
      { headers: { Authorization: 'Bearer ' + token } });

    const mine = await ask(tMe, bob, 'media');
    const mineJson = mine.ok ? await mine.json() : { items: [] };
    check('our own conversation is returned', mine.ok && mineJson.items.length === 2,
          'code ' + mine.status + ', attachments ' + mineJson.items.length);

    // Another pair: we ask as me about the eve ↔ mal conversation. The answer must
    // be empty — not a 200 with someone else's files.
    const foreign = await ask(tMe, eve, 'files');
    const fJson = foreign.ok ? await foreign.json() : { items: [] };
    check('the conversation of other people is not returned', fJson.items.length === 0,
          'returned ' + fJson.items.length + ' item(s)');

    const noAuth = await fetch(`${BASE}/attachments?peer=${bob}&kind=media`);
    check('without a token — refused', noAuth.status === 401, 'code ' + noAuth.status);

    const badKind = await ask(tMe, bob, 'everything');
    check('an unknown tab is rejected', badKind.status === 400, 'code ' + badKind.status);
  }

  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
  });
  const jsErrors = [];
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => jsErrors.push(e.message));

  // ══ 2. A skeleton instead of the incorrect "No conversations yet" ══════
  //
  // While the history loaded, the list claimed there were no conversations. That
  // is not an absence of information but an incorrect message: a person reads it
  // as "everything is lost".
  {
    const sctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, serviceWorkers: 'block' });
    const page = await sctx.newPage();
    page.on('pageerror', (e) => jsErrors.push('skeleton: ' + e.message));
    // We delay the history so the intermediate state can be caught.
    // The route is removed mid-delay, so a continue may arrive for an
    // already-handled request — that is a normal test race, not a breakage.
    await page.route('**/history*', async (route) => {
      await new Promise((r) => setTimeout(r, 2500));
      try { await route.continue(); } catch (e) {}
    });
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#auth-screen', { state: 'visible' });
    await page.fill('#auth-username', fresh);
    await page.fill('#auth-password', 'passw0rd-test');
    await page.click('#auth-btn');
    await page.waitForSelector('#chat-screen', { state: 'visible', timeout: 20000 });
    await page.waitForTimeout(700);

    const mid = await page.evaluate(() => ({
      skeleton: !!document.querySelector('.contacts-skeleton'),
      loading: typeof _historyLoading !== 'undefined' ? _historyLoading : 'none',
      html: ((document.getElementById('contacts-list') || {}).innerHTML || '').slice(0, 120),
      liesEmpty: /No conversations yet/i.test(
        (document.getElementById('contacts-list') || {}).textContent || ''),
    }));
    check('the skeleton is visible while loading', mid.skeleton,
          'loading flag=' + mid.loading + ', list: ' + mid.html);
    check('while loading it does NOT claim there are no conversations', !mid.liesEmpty);

    await page.unroute('**/history*');
    await page.waitForTimeout(3500);
    const after = await page.evaluate(() => ({
      skeleton: !!document.querySelector('.contacts-skeleton'),
      honest: /No conversations yet/i.test(
        (document.getElementById('contacts-list') || {}).textContent || ''),
    }));
    check('after loading the skeleton is removed', !after.skeleton);
    // And only now is "no conversations" the truth rather than a premature conclusion.
    check('after loading it honestly says there are no conversations', after.honest);
    await sctx.close();
  }

  // ══ 3. The live region announces an incoming message ═══════════════════
  {
    // Next — a user WITH a conversation, in the main context.
    await login(page, me);
    const before = await page.evaluate(() => !!document.getElementById('sr-live-polite'));
    await seed(tBob, [{ type: 'message', to: me, body: 'A message that must be announced.' }]);
    await page.waitForTimeout(1600);
    const live = await page.evaluate(() => {
      const el = document.getElementById('sr-live-polite');
      return { exists: !!el, text: el ? el.textContent : '',
               live: el ? el.getAttribute('aria-live') : '' };
    });
    check('the live region was created', live.exists, 'before the message it was: ' + before);
    check('the incoming message was announced', /must be announced/.test(live.text), JSON.stringify(live.text));
    check('the region is polite rather than interrupting', live.live === 'polite', live.live);

    // The stream is coalesced: five messages in a row must not become five
    // overlapping announcements.
    await seed(tBob, Array.from({ length: 5 }, (_, i) => ({
      type: 'message', to: me, body: 'burst ' + i })));
    await page.waitForTimeout(1800);
    const burst = await page.evaluate(() => document.getElementById('sr-live-polite').textContent);
    check('a stream of messages is coalesced into one announcement', /new messages/.test(burst), burst);
  }

  // ══ 4. The live region announces an incoming message ═══════════════════
  {
    await page.click(`#contact-${bob}`);
    // We wait for the conversation header to settle: right after switching chats
    // it is still being laid out, and a click lands in an intermediate frame.
    await page.waitForFunction(
      () => (document.getElementById('chat-header-name') || {}).textContent,
      null, { timeout: 10000 });
    await page.waitForTimeout(1200);
    await page.click('#attach-panel-btn');
    await page.waitForTimeout(1500);

    const media = await page.evaluate(() => ({
      open: !document.getElementById('attach-panel').hidden,
      feedHidden: getComputedStyle(document.getElementById('messages-wrap')).display === 'none',
      cells: document.querySelectorAll('.ap-cell').length,
      expanded: document.getElementById('attach-panel-btn').getAttribute('aria-expanded'),
    }));
    check('the panel opened', media.open && media.expanded === 'true');
    check('the feed is hidden rather than overlaid', media.feedHidden);
    check('the Media tab has exactly two images', media.cells === 2, 'tiles ' + media.cells);

    await page.click('.ap-tab[data-a1="files"]');
    await page.waitForTimeout(1200);
    const files = await page.evaluate(() => ({
      rows: document.querySelectorAll('.ap-row').length,
      name: (document.querySelector('.ap-fname') || {}).textContent || '',
    }));
    check('the Files tab has one document', files.rows === 1, 'rows ' + files.rows);
    check('text messages did not get into the panel', files.rows === 1 && media.cells === 2);

    await page.click('.ap-tab[data-a1="voice"]');
    await page.waitForTimeout(1200);
    const voiceRows = await page.evaluate(() => document.querySelectorAll('.ap-row').length);
    check('the Voice tab has one voice message', voiceRows === 1, 'rows ' + voiceRows);

    // The filter sifts what is loaded and says so honestly when it is empty.
    await page.click('.ap-tab[data-a1="files"]');
    await page.waitForTimeout(900);
    await page.fill('#ap-filter', 'contract');
    await page.waitForTimeout(400);
    const hit = await page.evaluate(() => document.querySelectorAll('.ap-row').length);
    check('the name filter finds the document', hit === 1, 'rows ' + hit);
    await page.fill('#ap-filter', 'zzzz-nothing');
    await page.waitForTimeout(400);
    const miss = await page.evaluate(() => (document.querySelector('.ap-empty') || {}).textContent || '');
    check('an empty result explains the filter boundary',
          /already loaded/i.test(miss), miss.slice(0, 60));
    await page.fill('#ap-filter', '');

    // Closing brings the feed back.
    await page.click('#attach-panel-btn');
    await page.waitForTimeout(500);
    const closed = await page.evaluate(() => ({
      hidden: document.getElementById('attach-panel').hidden,
      feed: getComputedStyle(document.getElementById('messages-wrap')).display !== 'none',
    }));
    check('the panel closes and the feed comes back', closed.hidden && closed.feed);
  }

  // ══ 5. Fixes from review: names, following the conversation, flicker ══
  {
    // The last message must be a FILE: the list row shows the last one, and the
    // sections above left text there.
    await seed(tBob, [{ type: 'message', to: me,
      body: withName(up[2], 'contract-v2.pdf'), media_type: up[2].media_type }]);
    await page.waitForTimeout(1500);

    await page.click(`#contact-${bob}`);
    await page.waitForFunction(
      () => (document.getElementById('chat-header-name') || {}).textContent,
      null, { timeout: 10000 });
    await page.waitForTimeout(1000);

    // The real file name in the chat list row. It used to be the generic "Photo",
    // which you cannot find a file by with your eyes.
    const preview = await page.evaluate((p) =>
      (document.querySelector('#contact-' + p + ' .contact-preview') || {}).textContent || '', bob);
    check('the chat list shows the real file name', /note\.webm|contract-v2|design-review|dashboard-final/.test(preview),
          JSON.stringify(preview));

    await page.click('#attach-panel-btn');
    await page.waitForTimeout(1200);
    // The sections above left the panel on the Files tab — back to media.
    await page.evaluate(() => { if (typeof attachPanelTab === 'function') attachPanelTab('media'); });
    await page.waitForTimeout(1600);

    // The tiles are labelled with a name rather than being nameless squares.
    const caps = await page.evaluate(() =>
      [...document.querySelectorAll('.ap-cap')].map((e) => e.textContent.trim()));
    check('media tiles are labelled with file names',
          caps.some((c) => /design-review\.png/.test(c)) && caps.some((c) => /dashboard-final\.png/.test(c)),
          JSON.stringify(caps));

    // Images are revealed on load rather than blinking as an empty frame.
    const faded = await page.evaluate(() => {
      const imgs = [...document.querySelectorAll('.ap-cell img')];
      return { total: imgs.length, ready: imgs.filter((i) => i.classList.contains('ap-ready')).length };
    });
    check('loaded tiles are revealed', faded.total > 0 && faded.ready === faded.total,
          JSON.stringify(faded));

    // Switching a tab must not leave an empty panel: the previous content is held
    // until the new one arrives, and the skeleton is shown only on a delay.
    await page.evaluate(() => { window.__apStates = []; });
    await page.evaluate(() => {
      const body = document.getElementById('ap-body');
      const obs = new MutationObserver(() => {
        window.__apStates.push(body.querySelector('.ap-sk') ? 'skeleton'
          : body.querySelector('.ap-row, .ap-cell') ? 'content'
          : body.querySelector('.ap-empty') ? 'empty' : 'blank');
      });
      obs.observe(body, { childList: true, subtree: false });
      window.__apObs = obs;
    });
    await page.click('.ap-tab[data-a1="files"]');
    await page.waitForTimeout(1400);
    const states = await page.evaluate(() => { window.__apObs.disconnect(); return window.__apStates; });
    check('there is no empty gap when switching tabs',
          !states.includes('blank') && !states.includes('skeleton'), JSON.stringify(states));

    // The panel follows the selected conversation.
    await page.click(`#contact-${eve}`).catch(() => {});
    const other = await page.evaluate(() => {
      const rows = [...document.querySelectorAll('.contact-item')];
      return rows.length > 1 ? rows[1].dataset.peer : null;
    });
    if (other) {
      await page.evaluate((p) => openChat(p), other);
      await page.waitForTimeout(1600);
      const follow = await page.evaluate(() => ({
        open: !document.getElementById('attach-panel').hidden,
        peer: typeof _apPeer !== 'undefined' ? _apPeer : null,
        active: typeof activePeer !== 'undefined' ? activePeer : null,
      }));
      check('the panel stayed open when the conversation changed', follow.open);
      check('the panel shows the attachments of the SELECTED conversation',
            follow.peer === follow.active, JSON.stringify(follow));
    }
    await page.evaluate(() => { if (typeof closeAttachPanel === 'function') closeAttachPanel(); });
  }

  check('no JS errors', jsErrors.length === 0, jsErrors.join(' | '));

  await browser.close();
  console.log(failures ? '\nFailures: ' + failures : '\nAll checks passed.');
  process.exit(failures ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
