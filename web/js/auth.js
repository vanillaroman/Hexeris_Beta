// Hexeris — auth: login/register/google + session restore.


// showAuthScreen — show the sign-in form once the config has been applied.
// The order matters: first the markup is brought to its final shape (tabs, the
// Google block), then it is revealed, and only then the loading screen is
// removed. Otherwise the user watches the card being assembled.
async function showAuthScreen() {
  try { await window.configReady; } catch {}
  const scr = document.getElementById('auth-screen');
  if (!scr) { _hideLoading(); return; }
  // The second-step screen may have stayed open — after signing out on it, for
  // instance. Otherwise the two screens would overlap.
  const tfa = document.getElementById('twofa-screen');
  if (tfa) tfa.style.display = 'none';
  scr.style.display = 'flex';

  const overlay = document.getElementById('app-loading');
  if (overlay) {
    // Application start. The form is under an opaque overlay — there is nobody
    // and no reason to fade it in: we put up a finished frame at once and only
    // dim the overlay. Two fades used to overlap, and in the intermediate frames
    // the spinner hung over a semi-transparent form — that was the flicker.
    scr.classList.add('instant', 'ready');
    // Two frames: the first applies the class, by the second the form is drawn.
    requestAnimationFrame(() => requestAnimationFrame(() => {
      scr.classList.remove('instant');
      _hideLoading();
    }));
    return;
  }
  // Signing out: there is no overlay, so the form appears with a transition.
  // A frame for display to apply — without it the browser coalesces both
  // changes and there is no animation.
  requestAnimationFrame(() => scr.classList.add('ready'));
}

function _hideLoading() {
  const el = document.getElementById('app-loading');
  if (!el) return;
  // The spinner is removed instantly rather than together with the overlay:
  // through the 150 ms fade it stayed visible on top of an already finished
  // screen — a spinning ring in the middle of the sign-in form was exactly the
  // "blink" visible on a recording.
  const sp = document.getElementById('app-loading-spinner');
  if (sp) sp.style.display = 'none';
  el.style.opacity = '0';
  setTimeout(() => el.remove(), 160);
}

async function tryRestoreSession() {
  // The return from the provider is handled BEFORE the saved session is read:
  // exchanging the code puts a token into the same storage, and from there the
  // path is the usual one. Otherwise someone who signed in through SSO would see
  // the sign-in screen on top of a session that had just been issued.
  try { await completeSSO(); } catch {}

  const savedToken = localStorage.getItem('hc_token');
  const savedUser  = localStorage.getItem('hc_user');
  if (!savedToken || !savedUser) {
    showAuthScreen();
    initSSO();
    return;
  }
  try {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 5000);
    const resp = await fetch(`${location.protocol}//${SERVER}/history?since=999999999&limit=1`, {
      headers: { 'Authorization': `Bearer ${savedToken}` },
      signal: controller.signal
    });
    clearTimeout(timeout);
    if (!resp.ok) { clearSession(); showAuthScreen(); initSSO(); return; }
    token = savedToken;
    myUsername = savedUser;
    // If the tab was closed without changing the admin-issued password, the
    // screen must appear again — otherwise the requirement is bypassed by a
    // page reload.
    try {
      const pr = await fetch(`${location.protocol}//${SERVER}/api/profile`, {
        headers: { 'Authorization': `Bearer ${savedToken}` }
      });
      if (pr.ok && (await pr.json()).must_change_password) {
        _hideLoading();
        return showPasswordChange();
      }
    } catch {}
    // Migration: sessions signed in before the auth cookie existed do not have
    // it — and without it <img>/<video> on /files/ get a 401. Fire-and-forget.
    fetch(`${location.protocol}//${SERVER}/api/session-cookie`, {
      method: 'POST',
      headers: { 'Authorization': `Bearer ${token}` }
    }).catch(() => {});
    startChat();
  } catch { clearSession(); showAuthScreen(); }
}

// ── Auth ──────────────────────────────────────────────────
function switchTab(mode) {
  authMode = mode;
  document.getElementById('tab-login').classList.toggle('active', mode === 'login');
  document.getElementById('tab-register').classList.toggle('active', mode === 'register');
  document.getElementById('auth-btn').textContent = mode === 'login' ? 'Sign in' : 'Create account';
  document.getElementById('auth-error').style.display = 'none';
}

// Signing in takes noticeably longer than an ordinary request: the password is
// checked with bcrypt, and that is deliberate. While there is no answer the
// button looks pressed but nothing happens, so the person presses again — and a
// second /login goes out. It was visible in the sign-in log: two records of one
// sign-in to the second.
//
// The consequences are not cosmetic: every repeat is another expensive bcrypt
// check on the server and another draw on the per-IP /login limit, so the person
// brings their own lockout closer with their own hands. And sign-ins cannot be
// counted from such a log.
//
// A flag rather than only the button's disabled state: doAuth is also called by
// Enter in the password field, where a disabled button changes nothing.
let _authInFlight = false;

async function doAuth() {
  if (_authInFlight) return;
  const username = document.getElementById('auth-username').value.trim();
  const password = document.getElementById('auth-password').value;
  if (!username || !password) return showError('Fill in all fields');

  const endpoint = authMode === 'login' ? 'login' : 'register';
  const btn = document.getElementById('auth-btn');
  _authInFlight = true;
  if (btn) {
    btn.disabled = true;
    // We change the label rather than merely dimming the button: "nothing is
    // happening" and "the check is running" must look different, otherwise
    // tapping again stays the most sensible thing to do from a person's point
    // of view.
    btn.textContent = authMode === 'login' ? 'Signing in…' : 'Creating account…';
  }
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/${endpoint}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password })
    });
    if (!resp.ok) { const t = await resp.text(); return showError(t.trim()); }
    const data = await resp.json();
    // Second factor. There is NO token in the response and there must not be —
    // the server sent a ticket that is good only for presenting a code.
    if (data.twofa_required) return showTwoFA(data.ticket, data.username);
    token = data.token;
    myUsername = data.username;
    saveSession();
    // The account was created by an administrator: the password is known to
    // more than its owner, so until it is changed we do not let them into
    // conversations.
    if (data.must_change_password) return showPasswordChange();
    startChat();
  } catch(e) {
    showError('Cannot connect to server');
  } finally {
    _authInFlight = false;
    if (btn) {
      btn.disabled = false;
      // The label is restored from authMode rather than from a remembered
      // string: the tab could have been switched while the request was in
      // flight, and the saved value would already be wrong.
      btn.textContent = authMode === 'login' ? 'Sign in' : 'Create account';
    }
  }
}

async function handleGoogleAuth(response) {
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/google-auth`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ credential: response.credential })
    });
    if (!resp.ok) { const t = await resp.text(); return showError(t.trim()); }
    const data = await resp.json();
    token = data.token;
    myUsername = data.username;
    saveSession();
    startChat();
  } catch(e) {
    showError('Google auth failed');
  }
}

function showError(msg) {
  const el = document.getElementById('auth-error');
  el.textContent = msg;
  el.style.display = 'block';
}



// ── Second sign-in step: the one-time code ────────────────
// The ticket lives in the tab's memory and is never persisted: in localStorage
// it would survive closing the tab, and that is a pass to enter a code without
// a password.
let _twofaTicket = null;
let _twofaInFlight = false;

function showTwoFA(ticket, username) {
  _twofaTicket = ticket;
  _hideLoading();
  document.getElementById('auth-screen').style.display = 'none';
  const scr = document.getElementById('twofa-screen');
  scr.style.display = 'flex';
  const who = document.getElementById('twofa-account');
  if (who) who.textContent = username || 'your account';
  const err = document.getElementById('twofa-error');
  if (err) err.style.display = 'none';
  const inp = document.getElementById('twofa-code');
  if (inp) { inp.value = ''; setTimeout(() => inp.focus(), 50); }
}

function showTwoFAError(msg) {
  const el = document.getElementById('twofa-error');
  el.textContent = msg;
  el.style.display = 'block';
}

// Going back to the sign-in screen throws the ticket away. Otherwise it would
// keep living in the tab's memory after the person changed their mind.
function cancelTwoFA() {
  _twofaTicket = null;
  document.getElementById('twofa-screen').style.display = 'none';
  const scr = document.getElementById('auth-screen');
  scr.style.display = 'flex';
  scr.classList.add('ready');
  const pw = document.getElementById('auth-password');
  if (pw) pw.value = '';
}

async function submitTwoFA() {
  if (_twofaInFlight) return;
  const raw = document.getElementById('twofa-code').value.trim();
  if (!raw) return showTwoFAError('Enter the code from your authenticator app');
  if (!_twofaTicket) return showTwoFAError('This sign-in attempt has expired — sign in again');

  const btn = document.getElementById('twofa-btn');
  _twofaInFlight = true;
  if (btn) { btn.disabled = true; btn.textContent = 'Checking…'; }
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/auth/2fa/verify`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ticket: _twofaTicket, code: raw })
    });
    if (!resp.ok) {
      const t = (await resp.text()).trim();
      // The ticket is single-use and has a limited number of attempts: once
      // the server has buried it, keeping the person on this screen is
      // pointless — any following code would fail too.
      if (resp.status === 401 && /expired/i.test(t)) {
        _twofaTicket = null;
        cancelTwoFA();
        return showError(t || 'Sign-in attempt expired — try again');
      }
      return showTwoFAError(t || 'Wrong code');
    }
    const data = await resp.json();
    _twofaTicket = null;
    token = data.token;
    myUsername = data.username;
    saveSession();
    document.getElementById('twofa-screen').style.display = 'none';
    // A recovery code is an event, not a routine: the list is finite, and the
    // person must learn it is shrinking at the moment they spend one.
    if (data.used_recovery_code) {
      alert('You signed in with a recovery code. ' + (data.recovery_left || 0) +
            ' left. Generate a new set in Settings once you have your authenticator app back.');
    }
    if (data.must_change_password) return showPasswordChange();
    startChat();
  } catch (e) {
    showTwoFAError('Cannot connect to server');
  } finally {
    _twofaInFlight = false;
    if (btn) { btn.disabled = false; btn.textContent = 'Verify'; }
  }
}

// ── Mandatory password change ─────────────────────────────
// A separate screen rather than a field in settings: until the temporary
// password is changed, the account effectively belongs to whoever issued it.
function showPasswordChange() {
  _hideLoading();
  document.getElementById('auth-screen').style.display = 'none';
  document.getElementById('pwchange-screen').style.display = 'flex';
  document.getElementById('pw-error').style.display = 'none';
  setTimeout(() => document.getElementById('pw-old').focus(), 50);
}

function showPwError(msg) {
  const el = document.getElementById('pw-error');
  el.textContent = msg;
  el.style.display = 'block';
}

async function submitPasswordChange() {
  const oldPw = document.getElementById('pw-old').value;
  const newPw = document.getElementById('pw-new').value;
  const rep   = document.getElementById('pw-new2').value;
  if (!oldPw || !newPw) return showPwError('Fill in all fields');
  if (newPw.length < 8) return showPwError('New password must be at least 8 characters');
  if (newPw !== rep)    return showPwError('Passwords do not match');

  const btn = document.getElementById('pw-btn');
  btn.disabled = true;
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/change-password`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` },
      body: JSON.stringify({ old_password: oldPw, new_password: newPw })
    });
    if (!resp.ok) { const t = await resp.text(); return showPwError(t.trim()); }
    // Changing the password revokes the previous tokens — the server issues a
    // new one straight away, otherwise the current tab would be signed out the
    // moment it succeeded.
    const data = await resp.json();
    if (data.token) { token = data.token; saveSession(); }
    document.getElementById('pwchange-screen').style.display = 'none';
    startChat();
  } catch (e) {
    showPwError('Cannot connect to server');
  } finally {
    btn.disabled = false;
  }
}
