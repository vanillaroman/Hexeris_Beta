// Hexeris — session persistence (token/user/active peer).

// ── Session persistence ───────────────────────────────────
function saveSession() {
  localStorage.setItem('hc_token', token);
  localStorage.setItem('hc_user', myUsername);
}

function saveActivePeer(peer) {
  if (peer) localStorage.setItem('hc_peer', peer);
  else localStorage.removeItem('hc_peer');
}

function clearSession() {
  // Remove only the authentication data — keep the encryption keys
  localStorage.removeItem('hc_token');
  localStorage.removeItem('hc_user');
  localStorage.removeItem('hc_peer');
}

function clearAllData() {
  // A full wipe happens only on Sign Out
  for (const key of Object.keys(localStorage)) {
    if (key.startsWith('hc_')) localStorage.removeItem(key);
  }
}

function loadActivePeer() {
  return localStorage.getItem('hc_peer') || null;
}
