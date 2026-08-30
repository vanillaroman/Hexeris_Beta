// The second factor: turning it on and off and recovery codes in settings.
// The second step of the sign-in itself lives in auth.js — there it is needed
// BEFORE a token exists, and it has nothing in common with this file.
//
// The window is built from markup in this file rather than from four hidden
// blocks in index.html: there are four states (off, setup, codes shown, turning
// off), and three of the four sets of ids would always be dead.

let _2faSecretShown = false;   // whether the secret is shown as text next to the QR
let _2faCodes = null;          // recovery codes, while the window is open

function _2faBox() { return document.getElementById('twofa-modal-box'); }

function _2faShow(html) {
  const overlay = document.getElementById('twofa-modal');
  _2faBox().innerHTML = html;
  overlay.style.display = 'flex';
}

function close2FAModal() {
  document.getElementById('twofa-modal').style.display = 'none';
  // The codes are dropped from memory together with the window: they have
  // already been shown once, and there is no reason to keep them.
  _2faCodes = null;
  _2faSecretShown = false;
}

// dismiss2FAOnEscape — close on Escape, but NOT on the recovery-codes step.
//
// close2FAModal wipes _2faCodes: the codes are shown exactly once and cannot be
// brought back once the window is closed — the person is left with the second
// factor enabled and not a single backup code, and if the phone is lost only an
// administrator can restore access. Escape gets pressed without looking, so on
// this step it does nothing: there is an explicit "Done" button right there.
// The other steps lose nothing and close as usual.
function dismiss2FAOnEscape() {
  // We look at what is on screen NOW rather than at the variable: _2faCodes
  // lives until the window closes and stays filled after moving to another
  // step — checking it locked Escape for the whole window right to the end.
  if (_2faBox() && _2faBox().querySelector('.dlg-codes')) return;
  close2FAModal();
}

function _2faError(msg) {
  const el = document.getElementById('twofa-modal-error');
  if (el) { el.textContent = msg; el.style.display = 'block'; }
}

async function _2faFetch(path, body) {
  const opts = {
    method: body === undefined ? 'GET' : 'POST',
    headers: { 'Authorization': 'Bearer ' + token }
  };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(`${location.protocol}//${SERVER}${path}`, opts);
  if (!r.ok) {
    const t = (await r.text()).trim();
    throw new Error(t || ('HTTP ' + r.status));
  }
  return r.json();
}

// ── Entry point from the settings menu ────────────────────
async function open2FASetup() {
  closeSettingsMenu();
  _2faShow(_2faHeader() + '<div class="dlg-text">Loading…</div>');
  try {
    const st = await _2faFetch('/auth/2fa/status');
    if (st.enabled) _2faRenderOn(st);
    else            await _2faRenderSetup();
  } catch (e) {
    _2faShow(_2faHeader() + '<div class="auth-error" style="display:block">' +
             escHtml(e.message) + '</div>');
  }
}

function _2faHeader() {
  return '<div class="gp-header"><div class="gp-title">Two-step verification</div>' +
         '<button class="gp-close" data-act="close2FAModal" aria-label="Close">✕</button></div>';
}

// ── Already on ────────────────────────────────────────────
function _2faRenderOn(st) {
  const left = st.recovery_left || 0;
  // Running out of recovery codes is a warning, not a statistics line: at zero,
  // losing the phone means a trip to the administrator.
  const warn = left === 0
    ? '<div class="auth-error" style="display:block">No recovery codes left. If you lose your ' +
      'phone, only an administrator can restore access.</div>'
    : (left <= 3
        ? '<div class="dlg-warn">Only ' + left + ' recovery codes left.</div>'
        : '<div class="dlg-text">' + left + ' recovery codes remaining.</div>');

  _2faShow(_2faHeader() +
    '<div class="dlg-text">Two-step verification is <b>on</b>. Signing in asks for ' +
    'a code from your authenticator app after your password.</div>' +
    warn +
    '<div class="auth-error" id="twofa-modal-error"></div>' +
    '<div class="dlg-actions"><button class="auth-btn" data-act="start2FADisable">Turn off</button></div>');
}

// ── Setup ─────────────────────────────────────────────────
async function _2faRenderSetup() {
  const s = await _2faFetch('/auth/2fa/setup', {});
  const qr = s.qr_png
    ? '<img class="dlg-qr" src="' + s.qr_png + '" alt="QR code for your authenticator app"/>'
    // The image did not come together — manual entry still works, and that is
    // better than an empty window with no explanation.
    : '<div class="dlg-warn">Could not draw the QR code — add the key below manually instead.</div>';

  _2faShow(_2faHeader() +
    '<div class="dlg-text">Scan this with Google Authenticator, Aegis, 1Password or any other ' +
    'authenticator app, then enter the 6-digit code it shows.</div>' +
    qr +
    '<div class="dlg-note">Can’t scan? Enter this key manually:' +
    '<code class="dlg-code">' + escHtml(s.secret) + '</code></div>' +
    '<input class="new-chat-input" id="twofa-setup-code" placeholder="6-digit code" ' +
    'inputmode="numeric" autocomplete="one-time-code" maxlength="7" ' +
    'data-act-enter="confirm2FAEnable"/>' +
    '<div class="auth-error" id="twofa-modal-error"></div>' +
    '<div class="dlg-actions"><button class="auth-btn" data-act="confirm2FAEnable">Turn on</button></div>');

  setTimeout(() => document.getElementById('twofa-setup-code')?.focus(), 60);
}

async function confirm2FAEnable() {
  const code = (document.getElementById('twofa-setup-code')?.value || '').trim();
  if (!code) return _2faError('Enter the code from your authenticator app');
  try {
    const res = await _2faFetch('/auth/2fa/enable', { code });
    _2faCodes = res.recovery_codes || [];
    _2faRenderCodes();
  } catch (e) {
    _2faError(e.message);
  }
}

// ── Recovery codes ────────────────────────────────────────
// Shown exactly once. The server does not store them in readable form, so
// "show them again" means reissuing, not repeating.
function _2faRenderCodes() {
  const list = (_2faCodes || []).map(c => '<div>' + escHtml(c) + '</div>').join('');

  _2faShow(_2faHeader() +
    '<div class="dlg-ok">Two-step verification is on.</div>' +
    '<div class="dlg-text">Save these recovery codes somewhere safe — a password manager or ' +
    'print them out. <b>They are shown only once</b> and each works a single time. Without ' +
    'them, losing your phone means asking an administrator to reset two-step verification.</div>' +
    '<div class="dlg-codes">' + list + '</div>' +
    '<div class="dlg-actions">' +
    '<button class="auth-btn" data-act="copyRecoveryCodes" id="twofa-copy">Copy codes</button>' +
    '<button class="auth-btn secondary" data-act="close2FAModal">Done</button></div>');
}

function copyRecoveryCodes() {
  const text = (_2faCodes || []).join('\n');
  const btn = document.getElementById('twofa-copy');
  const done = () => { if (btn) { btn.textContent = 'Copied'; setTimeout(() => btn.textContent = 'Copy codes', 2000); } };
  // navigator.clipboard is not available everywhere (http, old webviews) —
  // and doing nothing silently here is not an option, the person will never
  // see the codes again.
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(text).then(done, () => alert(text));
  } else {
    alert(text);
  }
}

// ── Turning off ───────────────────────────────────────────
// Password AND code — protection must not be undone with one click from an
// already open session. The server demands the same; the form merely keeps you
// from getting a refusal because of an empty field.
function start2FADisable() {
  _2faShow(_2faHeader() +
    '<div class="dlg-text">Turning this off means your password alone is enough to sign in. ' +
    'Confirm with your password and a current code.</div>' +
    '<input class="new-chat-input" type="password" id="twofa-off-pw" placeholder="Your password" ' +
    'autocomplete="current-password"/>' +
    '<input class="new-chat-input" id="twofa-off-code" placeholder="6-digit code or recovery code" ' +
    'autocomplete="one-time-code" data-act-enter="confirm2FADisable"/>' +
    '<div class="auth-error" id="twofa-modal-error"></div>' +
    '<div class="dlg-actions"><button class="auth-btn" data-act="confirm2FADisable">Turn off</button></div>');
  setTimeout(() => document.getElementById('twofa-off-pw')?.focus(), 60);
}

async function confirm2FADisable() {
  const password = document.getElementById('twofa-off-pw')?.value || '';
  const code = (document.getElementById('twofa-off-code')?.value || '').trim();
  if (!password || !code) return _2faError('Fill in both fields');
  try {
    await _2faFetch('/auth/2fa/disable', { password, code });
    close2FAModal();
    refresh2FALabel();
  } catch (e) {
    _2faError(e.message);
  }
}

// ── Menu item label ───────────────────────────────────────
// Otherwise the only way to learn whether the second factor is on is to open
// the window.
async function refresh2FALabel() {
  const el = document.querySelector('#settings-2fa .dd-label');
  if (!el || !token) return;
  try {
    const st = await _2faFetch('/auth/2fa/status');
    el.textContent = 'Two-step verification: ' + (st.enabled ? 'On' : 'Off');
  } catch { /* the label stays neutral — no reason to make noise */ }
}
