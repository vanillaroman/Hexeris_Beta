// Hexeris — auth: login/register/google + session restore.


// showAuthScreen displays the sign-in form once the config has been applied.
// The order matters: the markup reaches its final shape (tabs, Google block),
// then it appears, and only then the loading screen is removed. Otherwise the
// user watches the card assemble itself.
async function showAuthScreen() {
  try { await window.configReady; } catch {}
  const scr = document.getElementById('auth-screen');
  if (!scr) { _hideLoading(); return; }
  scr.style.display = 'flex';

  const overlay = document.getElementById('app-loading');
  if (overlay) {
    // Application start. The form sits under an opaque overlay, so there is
    // nothing to fade in: the finished frame is placed at once and only the
    // overlay fades. Two overlapping fades left intermediate frames with the
    // spinner over a half-transparent form, which is what flickered.
    // Two frames: the first applies the class, the second has it rendered.
    requestAnimationFrame(() => requestAnimationFrame(() => {
      scr.classList.remove('instant');
      _hideLoading();
    }));
    return;
  }
  // Signing out: no overlay, so the form fades in.
  // A frame to apply display, or the browser coalesces both changes and
  // there is no animation.
  requestAnimationFrame(() => scr.classList.add('ready'));
}

function _hideLoading() {
  const el = document.getElementById('app-loading');
  if (!el) return;
  // The spinner is removed instantly rather than with the overlay: during
  // the 150 ms fade it stayed visible over the finished screen, and a
  // spinning ring in the middle of the form was the flicker people saw.
  const sp = document.getElementById('app-loading-spinner');
  if (sp) sp.style.display = 'none';
  el.style.opacity = '0';
  setTimeout(() => el.remove(), 160);
}

async function tryRestoreSession() {
  const savedToken = localStorage.getItem('hc_token');
  const savedUser  = localStorage.getItem('hc_user');
  if (!savedToken || !savedUser) {
    showAuthScreen();
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
    if (!resp.ok) { clearSession(); showAuthScreen(); return; }
    token = savedToken;
    myUsername = savedUser;
    // If the tab was closed without changing an admin-issued password, the
    // screen must appear again, or a reload bypasses the requirement.
    try {
      const pr = await fetch(`${location.protocol}//${SERVER}/api/profile`, {
        headers: { 'Authorization': `Bearer ${savedToken}` }
      });
      if (pr.ok && (await pr.json()).must_change_password) {
        _hideLoading();
        return showPasswordChange();
      }
    } catch {}
    // Migration: sessions signed in before the auth cookie existed lack it,
    // and without it <img>/<video> on /files/ get a 401. Fire-and-forget.
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

async function doAuth() {
  const username = document.getElementById('auth-username').value.trim();
  const password = document.getElementById('auth-password').value;
  if (!username || !password) return showError('Fill in all fields');

  const endpoint = authMode === 'login' ? 'login' : 'register';
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/${endpoint}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password })
    });
    if (!resp.ok) { const t = await resp.text(); return showError(t.trim()); }
    const data = await resp.json();
    token = data.token;
    myUsername = data.username;
    saveSession();
    // The account was created by an admin, so someone other than the owner
    // knows the password: no access to conversations until it is changed.
    if (data.must_change_password) return showPasswordChange();
    startChat();
  } catch(e) {
    showError('Cannot connect to server');
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



// ── Mandatory password change ─────────────────────────────
// A separate screen rather than a field in settings: until the temporary
// password is replaced, the account belongs to whoever issued it.
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
    // Changing the password revokes the previous tokens, so the server
    // issues a new one immediately; otherwise this tab signs itself out.
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
