#!/usr/bin/env node
// Static checks of the admin panel (docs/admin-panel/admin-index.html).
//
// No browser or server is needed here: the panel is one self-contained file, and
// it breaks in predictable ways. What is checked is what cannot be spotted by eye
// while editing and what breaks the panel entirely:
//
//   1. The TABS list and the tab buttons have drifted apart. The active tab is
//      highlighted BY INDEX in TABS, so a button added without a row in the array
//      (or the other way round) shifts the highlight for every tab to the right —
//      and it does so silently, the panel keeps working.
//   2. A handler refers to a $('id') that is not in the markup. In JS that is
//      undefined and the button silently does nothing.
//   3. Duplicate ids: $() returns the first one, and an edit lands in the wrong place.
//   4. A button with onclick="name(...)" and no declared function.

const fs = require('fs');
const path = require('path');

const FILE = path.join(__dirname, '..', '..', 'docs', 'admin-panel', 'admin-index.html');
const html = fs.readFileSync(FILE, 'utf8');

let failed = 0;
function check(name, ok, detail) {
  if (ok) { console.log('  ok   ' + name); return; }
  failed++;
  console.log('  FAIL ' + name + (detail ? ' — ' + detail : ''));
}

// ── 1. Tabs ───────────────────────────────────────────────────────────────
const tabsDecl = /const TABS\s*=\s*\[([^\]]+)\]/.exec(html);
check('TABS is declared', !!tabsDecl);
const tabs = tabsDecl ? tabsDecl[1].split(',').map(s => s.trim().replace(/^['"]|['"]$/g, '')) : [];

const buttons = [...html.matchAll(/role="tab"[^>]*aria-controls="tab-([a-z-]+)"/g)].map(m => m[1]);
check('the button order matches TABS',
      JSON.stringify(tabs) === JSON.stringify(buttons),
      'TABS=' + JSON.stringify(tabs) + ' buttons=' + JSON.stringify(buttons));

for (const t of tabs) {
  check('the tab-' + t + ' panel exists', html.includes('id="tab-' + t + '"'));
}

// A loader is not required for every tab, but if one is declared the function
// must exist, otherwise opening the tab kills the handler.
const loaders = /const TAB_LOADERS\s*=\s*\{([\s\S]*?)\n\};/.exec(html);
check('TAB_LOADERS is declared', !!loaders);
if (loaders) {
  for (const m of loaders[1].matchAll(/([a-zA-Z_$][\w$]*)\s*\(/g)) {
    const fn = m[1];
    if (fn === 'if' || fn === 'for') continue;
    check('the tab loader calls an existing ' + fn + '()',
          new RegExp('function\\s+' + fn + '\\s*\\(').test(html));
  }
}

// ── 2. Duplicate ids ──────────────────────────────────────────────────────
const ids = [...html.matchAll(/\sid="([^"]+)"/g)].map(m => m[1]);
const dupes = ids.filter((v, i) => ids.indexOf(v) !== i);
check('no duplicate ids', dupes.length === 0, [...new Set(dupes)].join(', '));

// ── 3. $('id') points at an existing element ──────────────────────────────
const idSet = new Set(ids);
const missing = new Set();
for (const m of html.matchAll(/\$\('([a-z0-9-]+)'\)/g)) {
  if (!idSet.has(m[1])) missing.add(m[1]);
}
check('every $(id) exists in the markup', missing.size === 0, [...missing].join(', '));

// ── 4. onclick refers to an existing function ─────────────────────────────
const KEYWORDS = new Set(['if', 'for', 'while', 'switch', 'return', 'typeof']);
// A declaration can be either `function f(` or `const f = ` — the latter is the
// case for single-letter helpers such as $.
const declared = n => new RegExp('(function\\s+|const\\s+|let\\s+|var\\s+)' +
  n.replace(/\$/g, '\\$') + '\\s*[=(]').test(html);
const missingFns = new Set();
for (const m of html.matchAll(/onclick="([a-zA-Z_$][\w$]*)\s*\(/g)) {
  if (!KEYWORDS.has(m[1]) && !declared(m[1])) missingFns.add(m[1]);
}
check('every onclick handler is declared', missingFns.size === 0, [...missingFns].join(', '));

// ── 5. Conversation export ────────────────────────────────────────────────
// The only panel endpoint that returns message contents. What matters here is not
// "does the button work" but that it cannot be pressed blindly: without an
// employee, without a reason and without a confirmation.
const mx = /async function exportMessages\(btn\)\{([\s\S]*?)\n\}/.exec(html);
check('exportMessages is declared', !!mx);
if (mx) {
  const body = mx[1];
  const fetchAt = body.indexOf('fetch(');
  check('the request goes to /message-export', body.includes("'/message-export?'"));
  check('without an employee no request goes out',
        body.indexOf('if (!user)') >= 0 && body.indexOf('if (!user)') < fetchAt);
  check('without a reason no request goes out',
        body.indexOf('reason.length < 8') >= 0 && body.indexOf('reason.length < 8') < fetchAt);
  check('a confirmation is asked for before the export',
        body.indexOf('confirm(') >= 0 && body.indexOf('confirm(') < fetchAt);
  check('a truncated export is not returned silently', body.includes('X-Hexeris-Truncated'));
  check('the reason is stated to be written to the log', /Audit Log/.test(body));
}

// The path must match the server route — otherwise the button gives a 404 and
// the cause gets looked for in nginx.
const routes = fs.readFileSync(path.join(__dirname, '..', '..', 'server', 'main.go'), 'utf8');
check('the /admin/message-export route exists on the server',
      routes.includes('"/admin/message-export"'));

console.log(failed ? '\nFailures: ' + failed : '\nAll checks passed.');
process.exit(failed ? 1 : 0);
