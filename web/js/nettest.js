// Hexeris — Network test.
// Checks what matters for calls: server reachability, TURN credentials, and
// whether the browser actually gathers srflx (STUN) and relay (TURN) ICE
// candidates. A relay candidate confirms that calls will get through a strict
// NAT or firewall.

function openNetworkTest() {
  const ov = document.getElementById('nettest-modal-overlay');
  if (!ov) return;
  ov.classList.add('open');
  runNetworkTest();
}

function closeNetworkTest() {
  const ov = document.getElementById('nettest-modal-overlay');
  if (ov) ov.classList.remove('open');
}

function _ntSet(id, state, detail) { // state: wait | ok | fail
  const row = document.getElementById(id);
  if (!row) return;
  row.className = 'nt-row nt-' + state;
  const d = row.querySelector('.nt-detail');
  if (d && detail !== undefined) d.textContent = detail;
}

async function runNetworkTest() {
  const btn = document.getElementById('nt-run');
  if (btn) { btn.disabled = true; btn.textContent = 'Testing…'; }
  ['nt-server', 'nt-turn', 'nt-stun', 'nt-relay'].forEach(id => _ntSet(id, 'wait', '…'));

  // 1. Server and TURN credentials.
  try {
    const r = await fetch(`${location.protocol}//${SERVER}/turn-credentials`, {
      headers: { 'Authorization': 'Bearer ' + token }
    });
    if (r.ok) {
      const t = await r.json();
      _ntSet('nt-server', 'ok', 'reachable');
      const nUrls = Array.isArray(t.urls) ? t.urls.length : (t.urls ? 1 : 0);
      _ntSet('nt-turn', nUrls ? 'ok' : 'fail', nUrls ? nUrls + ' URL(s)' : 'not configured');
    } else {
      // 503 = TURN is not configured (calls fall back to STUN/host only).
      _ntSet('nt-server', r.status === 503 ? 'ok' : 'fail', 'HTTP ' + r.status);
      _ntSet('nt-turn', 'fail', r.status === 503 ? 'not configured' : 'HTTP ' + r.status);
    }
  } catch {
    _ntSet('nt-server', 'fail', 'unreachable');
    _ntSet('nt-turn', 'fail', '—');
  }

  // 2. ICE gathering (srflx = STUN, relay = TURN).
  try {
    const config = (typeof getIceServers === 'function')
      ? await getIceServers()
      : { iceServers: [{ urls: 'stun:stun.l.google.com:19302' }] };
    await _gatherIce(config);
  } catch {
    _ntSet('nt-stun', 'fail', 'error');
    _ntSet('nt-relay', 'fail', 'error');
  }

  if (btn) { btn.disabled = false; btn.textContent = 'Run again'; }
}

function _gatherIce(config) {
  return new Promise((resolve) => {
    let pc;
    try { pc = new RTCPeerConnection(config); }
    catch {
      _ntSet('nt-stun', 'fail', 'no WebRTC');
      _ntSet('nt-relay', 'fail', 'no WebRTC');
      return resolve();
    }
    let srflx = null, relay = null, finished = false;
    const finish = () => {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      _ntSet('nt-stun', srflx ? 'ok' : 'fail', srflx || 'no reflexive candidate');
      _ntSet('nt-relay', relay ? 'ok' : 'fail', relay || 'no relay candidate');
      try { pc.close(); } catch {}
      resolve();
    };
    const timer = setTimeout(finish, 6000);
    pc.onicecandidate = (e) => {
      if (!e.candidate) return finish(); // gathering complete
      const parts = (e.candidate.candidate || '').split(' ');
      const typIdx = parts.indexOf('typ');
      const type = typIdx >= 0 ? parts[typIdx + 1] : '';
      const ip = parts[4] || '';
      if (type === 'srflx' && !srflx) srflx = ip;
      if (type === 'relay' && !relay) relay = ip;
      if (srflx && relay) finish();
    };
    try {
      pc.createDataChannel('nt');
      pc.createOffer().then(o => pc.setLocalDescription(o)).catch(() => {});
    } catch {}
  });
}
