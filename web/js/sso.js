// Sign-in through a corporate provider (OIDC).
//
// The logic is deliberately thin: the browser goes to the provider and comes
// back with a ONE-TIME code in the address. The token itself never reaches the
// address — it would settle in the nginx log, in browser history and in the
// Referer, and would live there for all 30 days of its lifetime.

'use strict';

// The button is shown by applyAppConfig (js/appconfig.js) — together with the
// other sign-in methods and, importantly, IN BOTH DIRECTIONS: the previous
// initSSO could only show the button, so SSO disabled on the server stayed on
// screen until the page was reloaded.
//
// initSSO is kept as a thin wrapper: it is called from page start-up, and
// there is no reason to teach that code about the new module.
async function initSSO() {
  return refreshAppConfig(true);
}

function startSSO() {
  location.href = '/auth/oidc/start';
}

// Return from the provider. We exchange the one-time code for an ordinary
// Hexeris token and continue along the same path as a password sign-in.
async function completeSSO() {
  const params = new URLSearchParams(location.search);
  const err = params.get('sso_error');
  const code = params.get('sso');
  if (!err && !code) return false;

  // The address is cleaned IMMEDIATELY: the code is single-use, but there is
  // no reason to leave it in history or in the Referer.
  history.replaceState(null, '', location.pathname);

  if (err) {
    showSSOError(err);
    return false;
  }
  try {
    const res = await fetch('/auth/oidc/exchange', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code }),
    });
    if (!res.ok) {
      showSSOError((await res.text()).trim() || 'Single sign-on failed.');
      return false;
    }
    const data = await res.json();
    token = data.token;
    myUsername = data.username;
    saveSession();
    return true;
  } catch (e) {
    showSSOError('Single sign-on could not be completed.');
    return false;
  }
}

// The reason is shown in the same place as ordinary sign-in errors.
// The text comes from the server and is inserted as TEXT: it travels through
// the address bar, which means anyone can control it.
function showSSOError(text) {
  const box = document.getElementById('auth-error');
  if (box) {
    box.textContent = text;
    box.style.display = 'block';
    return;
  }
  alert(text);
}
