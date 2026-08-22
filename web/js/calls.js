// Hexeris — WebRTC calls. Reliability goals:
// • Safari/iOS: check both connectionState and iceConnectionState
// • Mobile Chrome: signal over wsSend, so candidates survive a WS reconnect
// • Firefox: a tolerant state machine (states arrive in any order)
// • All: a keep-alive silence track, ICE throttling, one cleanup path

// ── State ────────────────────────────────────────────────
let peerConnection = null;
let localStream    = null;
let callPeer       = null;
let callIsVideo    = false;
let isMuted        = false;
let isSpeaker      = true;
let pendingOffer   = null;
let iceCandidateBuffer = [];

let callTimerInterval = null;
let callSeconds       = 0;
let _isCaller         = false;
let _ringTimer        = null;
let _iceGraceTimer    = null;
let _restartTried     = false;
let _callConnected    = false; // has the timer started (guards double start)
let callTimeoutId     = null;
let _callLogged       = false; // call already logged (guards duplicates)
let _lastPairType     = null;  // selected ICE pair type: host/srflx/relay
let _curBitrate       = 0;     // current Opus bitrate ceiling (adaptive)
let _pendingCallIntent = null; // {from, action} from a push deep link

// ICE throttling: candidates are buffered and sent in batches every 150 ms.
// On a poor network hundreds of candidates otherwise flood the WS queue.
let _iceFlushTimer = null;
let _iceQueue      = [];
function _queueIce(candidate) {
  _iceQueue.push(candidate);
  if (!_iceFlushTimer) {
    _iceFlushTimer = setTimeout(() => {
      _iceFlushTimer = null;
      const batch = _iceQueue.splice(0);
      for (const c of batch) {
        wsSend({ type: 'call-ice', to: callPeer, body: JSON.stringify(c) });
      }
    }, 150);
  }
}

// ── Media ────────────────────────────────────────────────
function _callMediaConstraints() {
  return {
    audio: {
      echoCancellation: true, noiseSuppression: true, autoGainControl: true,
      channelCount: 1, sampleRate: 48000
    },
    video: callIsVideo
      ? { facingMode: 'user', width: { ideal: 1280 }, height: { ideal: 720 } }
      : false
  };
}

// ── Opus tuning ──────────────────────────────────────────
// Browsers default to Opus around 32 kbps without explicit FEC, so audio
// gargles and drops on packet loss. These SDP parameters add inband FEC
// (repairs isolated losses without retransmission), DTX (silence costs no
// bitrate), a higher ceiling and mono. Applied to offers, answers and ICE
// restarts alike.
function _tuneOpusSdp(sdp) {
  if (!sdp) return sdp;
  const m = /a=rtpmap:(\d+)\s+opus\/48000/i.exec(sdp);
  if (!m) return sdp;
  const pt = m[1];
  const extra = 'useinbandfec=1;usedtx=1;stereo=0;maxaveragebitrate=48000;maxplaybackrate=48000';
  const fmtpRe = new RegExp('a=fmtp:' + pt + ' ([^\\r\\n]*)', 'i');
  if (fmtpRe.test(sdp)) {
    return sdp.replace(fmtpRe, (line, params) => {
      // Do not duplicate keys the browser already set.
      const have = new Set(params.split(';').map(s => s.split('=')[0].trim()));
      const add = extra.split(';').filter(kv => !have.has(kv.split('=')[0]));
      return 'a=fmtp:' + pt + ' ' + params + (add.length ? ';' + add.join(';') : '');
    });
  }
  return sdp.replace(new RegExp('(a=rtpmap:' + pt + ' opus/48000[^\\r\\n]*)'), '$1\r\na=fmtp:' + pt + ' ' + extra);
}

// Adaptive bitrate: the encoder's ceiling drops as loss rises and recovers
// gradually, so quality does not swing on every tick.
function _setSenderBitrate(bps) {
  if (!peerConnection) return;
  const sender = peerConnection.getSenders().find(s => s.track && s.track.kind === 'audio' && s.track !== _silenceTrack);
  if (!sender || !sender.getParameters) return;
  try {
    const p = sender.getParameters();
    if (!p.encodings || !p.encodings.length) p.encodings = [{}];
    p.encodings[0].maxBitrate = bps;
    sender.setParameters(p).catch(() => {});
  } catch {}
}

// Keep-alive: a silence track stops the browser ending the media session
// after ~30 s of real silence, which matters most on iOS in the background.
let _silenceTrack = null;
function _createSilenceTrack() {
  if (_silenceTrack) return _silenceTrack;
  const ctx = new (window.AudioContext || window.webkitAudioContext)();
  const dst = ctx.createMediaStreamDestination();
  const osc = ctx.createOscillator();
  const gain = ctx.createGain();
  gain.gain.value = 0; // zero volume — the track is inaudible
  osc.connect(gain);
  gain.connect(dst);
  osc.start();
  _silenceTrack = dst.stream.getAudioTracks()[0];
  return _silenceTrack;
}

function _attachLocalVideo() {
  const lv = document.getElementById('local-video');
  if (callIsVideo && lv) { lv.srcObject = localStream; lv.play().catch(() => {}); }
}

function _onRemoteTrack(e) {
  const stream = e.streams[0];
  if (callIsVideo) {
    const rv = document.getElementById('remote-video');
    if (rv) { rv.srcObject = stream; rv.play().catch(() => {}); }
  } else {
    const audio = document.getElementById('remote-audio');
    audio.srcObject = stream;
    audio.play().catch(() => {});
  }
}

// ── ICE / TURN ───────────────────────────────────────────
const ICE_STUN_ONLY = {
  iceServers: [
    { urls: 'stun:stun.l.google.com:19302' },
    { urls: 'stun:stun1.l.google.com:19302' }
  ]
};

let _iceCache = null; // { config, expiresAt }

async function getIceServers() {
  if (_iceCache && Date.now() < _iceCache.expiresAt) return _iceCache.config;
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/turn-credentials`, {
      headers: { 'Authorization': `Bearer ${token}` }
    });
    if (!resp.ok) return ICE_STUN_ONLY;
    const t = await resp.json();
    const config = {
      iceServers: [
        ...ICE_STUN_ONLY.iceServers,
        { urls: t.urls, username: t.username, credential: t.credential }
      ]
    };
    _iceCache = { config, expiresAt: Date.now() + (t.ttl - 300) * 1000 };
    return config;
  } catch {
    return ICE_STUN_ONLY;
  }
}

// ── PeerConnection factory ───────────────────────────────
// Create the peer connection and attach every handler in one place.
// Safari often fires iceconnectionstatechange while staying quiet on
// connectionstatechange, and Firefox does the reverse, so both are observed.
async function _createPC() {
  const config = await getIceServers();
  const pc = new RTCPeerConnection(config);

  pc.onicecandidate = (e) => {
    if (e.candidate) _queueIce(e.candidate);
  };

  pc.ontrack = _onRemoteTrack;

  // One state handler covering connectionState and iceConnectionState
  function _onStateChange() {
    const cs  = pc.connectionState;
    const ice = pc.iceConnectionState;

    // Connected: start the timer exactly once
    const connected = cs === 'connected' || ice === 'connected' || ice === 'completed';
    if (connected && !_callConnected) {
      _callConnected = true;
      _clearCallTimers();
      startCallTimer();
      showCallOverlay(callPeer, 'Connected', 'active');
      return;
    }

    // Transient: give it a chance to recover
    if (cs === 'disconnected' || ice === 'disconnected') {
      if (pc.remoteDescription) handleIceTrouble();
      return;
    }

    // Terminal failures
    if (cs === 'failed' || ice === 'failed') {
      if (!_restartTried && _isCaller && pc.remoteDescription) {
        _tryIceRestart(pc);
      } else if (pc.remoteDescription) {
        cleanupCall();
      }
      return;
    }

    if (cs === 'closed') {
      if (pc.remoteDescription && _callConnected) cleanupCall();
    }
  }

  pc.onconnectionstatechange    = _onStateChange;
  pc.oniceconnectionstatechange = _onStateChange;

  return pc;
}

// ── ICE restart ──────────────────────────────────────────
async function _tryIceRestart(pc) {
  if (_restartTried) return;
  _restartTried = true;
  try {
    const offer = await pc.createOffer({ iceRestart: true });
    offer.sdp = _tuneOpusSdp(offer.sdp);
    await pc.setLocalDescription(offer);
    wsSend({ type: 'call-restart', to: callPeer, body: JSON.stringify(offer) });
    // 12 s to recover, then hang up
    _iceGraceTimer = setTimeout(() => {
      _iceGraceTimer = null;
      if (pc.connectionState !== 'connected' && pc.iceConnectionState !== 'connected') cleanupCall();
    }, 12000);
  } catch { cleanupCall(); }
}

function handleIceTrouble() {
  if (_iceGraceTimer || !peerConnection) return;
  showCallOverlay(callPeer, 'Reconnecting…', 'active');
  // 5 s of grace — a Wi-Fi→LTE handover disconnects for 1–3 s
  _iceGraceTimer = setTimeout(async () => {
    _iceGraceTimer = null;
    if (!peerConnection) return;
    if (peerConnection.connectionState === 'connected' || peerConnection.iceConnectionState === 'connected') {
      showCallOverlay(callPeer, 'Connected', 'active');
      return;
    }
    if (_isCaller) await _tryIceRestart(peerConnection);
    else {
      // the callee waits for a new offer from the caller (another 12 s)
      _iceGraceTimer = setTimeout(() => {
        _iceGraceTimer = null;
        if (peerConnection && peerConnection.connectionState !== 'connected') cleanupCall();
      }, 12000);
    }
  }, 5000);
}

function _clearCallTimers() {
  clearTimeout(_ringTimer);     _ringTimer     = null;
  clearTimeout(_iceGraceTimer); _iceGraceTimer = null;
  clearTimeout(callTimeoutId);  callTimeoutId  = null;
  clearTimeout(_iceFlushTimer); _iceFlushTimer = null;
  _iceQueue = [];
  _restartTried = false;
}

function _onIceHealthy() {
  _restartTried = false;
  if (_iceGraceTimer) { clearTimeout(_iceGraceTimer); _iceGraceTimer = null; }
}

// ── Placing a call ───────────────────────────────────────
async function startCall(video = false) {
  if (!activePeer || !ws) return;
  if (peerConnection) return;
  callIsVideo   = !!video;
  _callConnected = false;
  _callLogged    = false;

  try {
    localStream = await navigator.mediaDevices.getUserMedia(_callMediaConstraints());
  } catch(e) {
    toast('Cannot access ' + (callIsVideo ? 'camera/microphone' : 'microphone') + ': ' + e.message);
    callIsVideo = false;
    return;
  }

  peerConnection = await _createPC();
  localStream.getTracks().forEach(t => peerConnection.addTrack(t, localStream));
  // Keep-alive: add the silence track so a pause does not kill the session
  peerConnection.addTrack(_createSilenceTrack(), localStream);
  _attachLocalVideo();

  const offer = await peerConnection.createOffer();
  offer.sdp = _tuneOpusSdp(offer.sdp);
  await peerConnection.setLocalDescription(offer);

  wsSend({ type: 'call-offer', to: activePeer, body: JSON.stringify(offer) });
  showCallOverlay(activePeer, 'Calling…', 'calling');
  callPeer = activePeer;
  _isCaller = true;

  // 45 s of ringing — the same lifetime as the server's signal queue
  _ringTimer = setTimeout(() => {
    if (peerConnection && !peerConnection.remoteDescription) {
      _logCallOnce('missed', 0);
      showCallOverlay(callPeer, 'No answer', 'calling');
      setTimeout(endCall, 1500);
    }
  }, 45000);
}

// ── Answering ────────────────────────────────────────────
async function acceptCall() {
  if (!pendingOffer || !callPeer) return;
  callIsVideo    = /m=video/.test(pendingOffer.sdp || '');
  _callConnected = false;
  _callLogged    = false;

  try {
    localStream = await navigator.mediaDevices.getUserMedia(_callMediaConstraints());
  } catch(e) {
    // Camera unavailable — fall back to audio only
    if (callIsVideo) {
      try { localStream = await navigator.mediaDevices.getUserMedia({ audio: _callMediaConstraints().audio, video: false }); }
      catch(e2) { toast('Cannot access microphone: ' + e2.message); rejectCall(); return; }
    } else {
      toast('Cannot access microphone: ' + e.message); rejectCall(); return;
    }
  }

  peerConnection = await _createPC();
  localStream.getTracks().forEach(t => peerConnection.addTrack(t, localStream));
  peerConnection.addTrack(_createSilenceTrack(), localStream);
  _attachLocalVideo();

  await peerConnection.setRemoteDescription(new RTCSessionDescription(pendingOffer));
  pendingOffer = null;

  // Apply the buffered ICE candidates
  for (const c of iceCandidateBuffer) {
    try { await peerConnection.addIceCandidate(new RTCIceCandidate(c)); } catch {}
  }
  iceCandidateBuffer = [];

  const answer = await peerConnection.createAnswer();
  answer.sdp = _tuneOpusSdp(answer.sdp);
  await peerConnection.setLocalDescription(answer);
  _isCaller = false;
  wsSend({ type: 'call-answer', to: callPeer, body: JSON.stringify(answer) });
  showCallOverlay(callPeer, 'Connecting…', 'calling');
}

function rejectCall() {
  if (callPeer) wsSend({ type: 'call-reject', to: callPeer });
  cleanupCall();
}

function endCall() {
  if (callPeer) wsSend({ type: 'call-end', to: callPeer });
  cleanupCall();
}

// ── Cleanup ──────────────────────────────────────────────
function cleanupCall() {
  // The caller writes the completed call into the conversation (before the state reset below).
  if (_isCaller && _callConnected) _logCallOnce('done', callSeconds);
  stopVolumeIndicator();
  stopNetworkStats();
  if (_screenStream) { _screenStream.getTracks().forEach(t => t.stop()); _screenStream = null; _origVideoTrack = null; }
  _clearCallTimers();
  _callConnected = false;
  _isCaller      = false;
  _callLogged    = false;
  _lastPairType  = null;
  _curBitrate    = 0;
  if (peerConnection) { peerConnection.close(); peerConnection = null; }
  if (localStream) { localStream.getTracks().forEach(t => t.stop()); localStream = null; }
  const audio = document.getElementById('remote-audio');
  audio.srcObject = null;
  const rv = document.getElementById('remote-video'); if (rv) rv.srcObject = null;
  const lv = document.getElementById('local-video');  if (lv) { lv.srcObject = null; lv.style.opacity = '1'; }
  callIsVideo = false;
  callPeer    = null;
  // Panel and camera state last exactly one call: otherwise the next video
  // call would start with hidden buttons or a rear-camera label, and the
  // hide timer would still be pending.
  clearTimeout(_controlsTimer); _controlsTimer = null;
  _facingMode = 'user';
  _flipInProgress = false;
  document.getElementById('call-overlay').classList.remove(
    'video-mode', 'controls-visible', 'rear-camera', 'has-multi-cam');
  const camBtn = document.getElementById('camera-btn'); if (camBtn) camBtn.classList.remove('active');
  document.getElementById('camera-label').textContent = 'Camera';
  document.getElementById('camera-btn-wrap').style.display = 'none';
  const scrBtn = document.getElementById('screen-btn'); if (scrBtn) scrBtn.classList.remove('active');
  const scrLabel = document.getElementById('screen-label'); if (scrLabel) scrLabel.textContent = 'Share';
  const scrWrap = document.getElementById('screen-btn-wrap'); if (scrWrap) scrWrap.style.display = 'none';
  isMuted = false;
  pendingOffer = null;
  iceCandidateBuffer = [];
  _iceQueue = [];
  stopRingtone();
  document.getElementById('mute-btn').classList.remove('active');
  document.getElementById('speaker-btn').classList.remove('active');
  document.getElementById('speaker-label').textContent = 'Speaker';
  isSpeaker = true;
  document.getElementById('mute-label').textContent = 'Mute';
  document.getElementById('volume-slider').value = 100;
  document.getElementById('call-bar').classList.remove('visible');
  document.getElementById('minimize-call-btn').style.display = 'none';
  hideCallOverlay();
}

// ── Call log ─────────────────────────────────────────────
// The caller is the single source of truth: they write a system call message
// into the conversation (media_type:'call', body='<outcome>:<dur>:<video>')
// that both sides see in history. Missed calls therefore survive even when
// the recipient was offline, since delivery uses the ordinary reliable path.
function _logCallOnce(outcome, durSec) {
  if (_callLogged || !_isCaller || !callPeer) return;
  _callLogged = true;
  const peer = callPeer;
  const body = `${outcome}:${durSec || 0}:${callIsVideo ? 1 : 0}`;
  const msgID = Date.now() + '-' + Math.floor(Math.random() * 99999);
  const env = { type: 'message', id: msgID, from: myUsername, to: peer, body, media_type: 'call' };
  // Use queueMessage (reliable delivery) when available, otherwise wsSend.
  if (typeof queueMessage === 'function') queueMessage(env); else wsSend(env);
  if (typeof addToChat === 'function')
    addToChat(peer, { id: msgID, from: myUsername, to: peer, body, media_type: 'call', status: 'sending', ts: Date.now() });
  if (typeof updateContact === 'function') updateContact(peer, outcome === 'missed' ? '📞 Missed call' : '📞 Call');
  if (activePeer === peer && typeof renderMessages === 'function') renderMessages(peer);
}

// ── Deep link from the incoming-call push ────────────────
// sw.js opens /?call=<from>&callaction=accept|decline. The intent is stored
// and applied as soon as the call offer arrives, since the server flushes its
// queue when the socket comes up. The query is cleared from the URL at once.
function initCallDeepLink() {
  try {
    const p = new URLSearchParams(location.search);
    const from = p.get('call');
    const action = p.get('callaction');
    // Act automatically only on an explicit push choice (Accept/Decline on
    // Android and desktop). An ordinary notification tap — including iOS,
    // which has no buttons — just opens the app and shows the usual overlay.
    if (from && action) {
      _pendingCallIntent = { from, action };
      const url = location.pathname + location.hash;
      history.replaceState(null, '', url);
    }
  } catch {}
}
initCallDeepLink();

// ── Incoming signals ─────────────────────────────────────
async function handleCallMessage(msg) {
  if (msg.type === 'call-offer') {
    if (peerConnection) {
      wsSend({ type: 'call-reject', to: msg.from });
      return;
    }
    callPeer     = msg.from;
    pendingOffer = JSON.parse(msg.body);
    const isVideo = /m=video/.test(pendingOffer.sdp || '');
    showCallOverlay(msg.from, isVideo ? 'Incoming video call…' : 'Incoming call…', 'incoming');
    // Arrived from an Accept/Decline push — carry out the intent.
    if (_pendingCallIntent && _pendingCallIntent.from === msg.from) {
      const act = _pendingCallIntent.action; _pendingCallIntent = null;
      if (act === 'decline') { rejectCall(); return; }
      setTimeout(() => { if (pendingOffer) acceptCall(); }, 300);
    }
  }

  if (msg.type === 'call-answer' && peerConnection) {
    clearTimeout(_ringTimer); _ringTimer = null;
    await peerConnection.setRemoteDescription(new RTCSessionDescription(JSON.parse(msg.body)));
    for (const c of iceCandidateBuffer) {
      try { await peerConnection.addIceCandidate(new RTCIceCandidate(c)); } catch {}
    }
    iceCandidateBuffer = [];
  }

  if (msg.type === 'call-restart' && peerConnection && !_isCaller) {
    try {
      await peerConnection.setRemoteDescription(new RTCSessionDescription(JSON.parse(msg.body)));
      const answer = await peerConnection.createAnswer();
      answer.sdp = _tuneOpusSdp(answer.sdp);
      await peerConnection.setLocalDescription(answer);
      wsSend({ type: 'call-answer', to: callPeer, body: JSON.stringify(answer) });
      _restartTried = false;
    } catch {}
    return;
  }

  if (msg.type === 'call-ice') {
    const c = JSON.parse(msg.body);
    if (peerConnection && peerConnection.remoteDescription) {
      try { await peerConnection.addIceCandidate(new RTCIceCandidate(c)); } catch {}
    } else {
      iceCandidateBuffer.push(c);
    }
  }

  if (msg.type === 'call-end') { cleanupCall(); }
  if (msg.type === 'call-reject') {
    _logCallOnce('declined', 0);
    const el = document.getElementById('call-status');
    if (el) { el.textContent = 'Call declined'; el.style.display = 'block'; }
    setTimeout(cleanupCall, 1500);
  }
}

// Rendering a call message in the feed (media_type:'call', body='outcome:dur:video').
function callLogHtml(m, isOut) {
  const [outcome, durStr, vid] = String(m.body || '').split(':');
  const dur = parseInt(durStr) || 0;
  const isVideo = vid === '1';
  const mm = Math.floor(dur / 60), ss = dur % 60;
  const durTxt = dur ? mm + ':' + String(ss).padStart(2, '0') : '';
  let label, cls = 'call-log';
  if (outcome === 'missed')        { label = isOut ? 'No answer' : 'Missed call'; if (!isOut) cls += ' missed'; }
  else if (outcome === 'declined') { label = isOut ? 'Declined' : 'Declined'; }
  else                             { label = (isVideo ? 'Video call' : 'Call') + (durTxt ? ' · ' + durTxt : ''); }
  const arrow = isOut ? '↗' : '↘';
  const icon = isVideo
    ? '<svg viewBox="0 0 24 24"><polygon points="23 7 16 12 23 17 23 7"/><rect x="1" y="5" width="15" height="14" rx="2"/></svg>'
    : '<svg viewBox="0 0 24 24"><path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07A19.5 19.5 0 0 1 4.69 12 19.79 19.79 0 0 1 1.61 3.4 2 2 0 0 1 3.6 1.22h3a2 2 0 0 1 2 1.72 12.84 12.84 0 0 0 .7 2.81 2 2 0 0 1-.45 2.11L7.91 8.84a16 16 0 0 0 6 6l.94-.94a2 2 0 0 1 2.11-.45 12.84 12.84 0 0 0 2.81.7A2 2 0 0 1 22 16.92z"/></svg>';
  return `<span class="${cls}"><span class="call-log-ico">${icon}</span><span class="call-log-arrow">${arrow}</span>${escHtml(label)}</span>`;
}

// ── UI helpers ───────────────────────────────────────────
function showCallOverlay(peer, status, mode) {
  callPeer = peer;
  const overlay    = document.getElementById('call-overlay');
  const avatar     = document.getElementById('call-avatar');
  const avatarWrap = document.getElementById('call-avatar-wrap');
  const name       = document.getElementById('call-name');
  const statusEl   = document.getElementById('call-status');
  const timer      = document.getElementById('call-timer');
  const volume     = document.getElementById('call-volume');

  avatar.textContent = peer[0].toUpperCase();
  avatar.className   = 'call-avatar ' + avatarClass(peer);
  document.querySelectorAll('.call-pulse').forEach(p => p.style.color = getComputedStyle(avatar).backgroundColor);
  name.textContent      = peer;
  statusEl.textContent  = status;
  statusEl.style.display = 'block';
  timer.style.display    = 'none';
  volume.classList.remove('visible');

  document.getElementById('call-btns-calling').style.display  = mode === 'calling'  ? 'flex' : 'none';
  document.getElementById('call-btns-incoming').style.display = mode === 'incoming' ? 'flex' : 'none';
  document.getElementById('call-btns-active').style.display   = mode === 'active'   ? 'flex' : 'none';

  if (mode === 'active') volume.classList.add('visible');
  if (mode === 'calling' || mode === 'incoming') avatarWrap.classList.add('pulsing');
  else avatarWrap.classList.remove('pulsing');

  if (mode === 'incoming') playRingtone(); else stopRingtone();

  overlay.classList.toggle('video-mode', callIsVideo);
  document.getElementById('camera-btn-wrap').style.display = (callIsVideo && mode === 'active') ? 'flex' : 'none';
  const sbw = document.getElementById('screen-btn-wrap'); if (sbw) sbw.style.display = (callIsVideo && mode === 'active') ? 'flex' : 'none';
  overlay.classList.add('visible');
  // The panel is always shown when the call state changes (conversation
  // started, incoming call); hiding begins only during an active video call.
  showCallControls(callIsVideo && mode === 'active');
  if (callIsVideo && mode === 'active') updateFlipCameraAvailability();
}

function hideCallOverlay() {
  document.getElementById('call-overlay').classList.remove('visible');
  clearInterval(callTimerInterval); callTimerInterval = null;
  document.getElementById('call-timer').style.display = 'none';
  callSeconds = 0;
}

function startCallTimer() {
  const timer = document.getElementById('call-timer');
  timer.style.display = 'block';
  document.getElementById('call-status').style.display = 'none';
  document.getElementById('call-volume').classList.add('visible');
  document.getElementById('call-avatar-wrap').classList.remove('pulsing');
  document.getElementById('minimize-call-btn').style.display = 'block';
  document.getElementById('call-bar-name').textContent = callPeer;
  playConnectSound();
  // The volume indicator and network monitoring start exactly on connect
  // (these functions existed but were never called, so quality data was dead).
  startVolumeIndicator();
  startNetworkStats();
  callSeconds = 0;
  if (callTimerInterval) clearInterval(callTimerInterval);
  callTimerInterval = setInterval(() => {
    callSeconds++;
    const m = Math.floor(callSeconds / 60), s = callSeconds % 60;
    const str = m + ':' + String(s).padStart(2, '0');
    timer.textContent = str;
    document.getElementById('call-bar-timer').textContent = str;
  }, 1000);
}

function minimizeCall() {
  document.getElementById('call-overlay').classList.remove('visible');
  document.getElementById('call-bar').classList.add('visible');
}

function expandCall() {
  document.getElementById('call-bar').classList.remove('visible');
  document.getElementById('call-overlay').classList.add('visible');
}

// ── Audio and camera controls ────────────────────────────
function toggleMute() {
  if (!localStream) return;
  isMuted = !isMuted;
  localStream.getAudioTracks().forEach(t => { if (t !== _silenceTrack) t.enabled = !isMuted; });
  document.getElementById('mute-btn').classList.toggle('active', isMuted);
  document.getElementById('mute-label').textContent = isMuted ? 'Unmute' : 'Mute';
}

async function toggleSpeaker() {
  const audio = document.getElementById('remote-audio');
  isSpeaker = !isSpeaker;
  if (audio.setSinkId) {
    try {
      const devices = await navigator.mediaDevices.enumerateDevices();
      const outputs = devices.filter(d => d.kind === 'audiooutput');
      if (!isSpeaker) {
        const ear = outputs.find(d => /ear|handset|phone/i.test(d.label));
        if (ear) await audio.setSinkId(ear.deviceId);
      } else {
        await audio.setSinkId('default');
      }
    } catch(e) { console.warn('setSinkId:', e); }
  }
  document.getElementById('speaker-btn').classList.toggle('active', !isSpeaker);
  document.getElementById('speaker-label').textContent = isSpeaker ? 'Speaker' : 'Earpiece';
}

// ── Screen sharing ───────────────────────────────────────
let _screenStream = null;
let _origVideoTrack = null;

async function toggleScreenShare() {
  const btn = document.getElementById('screen-btn');
  const label = document.getElementById('screen-label');
  if (!peerConnection || !localStream) return;
  if (_screenStream) {
    _screenStream.getTracks().forEach(t => t.stop());
    _screenStream = null;
    if (_origVideoTrack) {
      const sender = peerConnection.getSenders().find(s => s.track && s.track.kind === 'video');
      if (sender) await sender.replaceTrack(_origVideoTrack);
      _origVideoTrack = null;
    }
    if (btn) btn.classList.remove('active');
    if (label) label.textContent = 'Share';
    return;
  }
  try {
    _screenStream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: false });
    const screenTrack = _screenStream.getVideoTracks()[0];
    _origVideoTrack = localStream.getVideoTracks()[0] || null;
    const sender = peerConnection.getSenders().find(s => s.track && s.track.kind === 'video');
    if (sender) await sender.replaceTrack(screenTrack);
    if (btn) btn.classList.add('active');
    if (label) label.textContent = 'Stop';
    screenTrack.onended = () => toggleScreenShare();
  } catch(e) { _screenStream = null; }
}

// ── Volume indicator ─────────────────────────────────────
let _volAnalyser = null, _volTimer = null;

function startVolumeIndicator() {
  if (!localStream) return;
  try {
    const ctx = getAudioCtx();
    const src = ctx.createMediaStreamSource(localStream);
    _volAnalyser = ctx.createAnalyser();
    _volAnalyser.fftSize = 256;
    src.connect(_volAnalyser);
    const buf = new Uint8Array(_volAnalyser.frequencyBinCount);
    const bar = document.getElementById('vol-bar');
    const ind = document.getElementById('volume-indicator');
    if (ind) ind.style.display = 'block';
    _volTimer = setInterval(() => {
      if (!_volAnalyser) return;
      _volAnalyser.getByteFrequencyData(buf);
      const vol = buf.reduce((a, b) => a + b, 0) / buf.length;
      if (bar) bar.style.width = Math.min(vol * 2, 100) + '%';
    }, 80);
  } catch {}
}

function stopVolumeIndicator() {
  if (_volTimer) { clearInterval(_volTimer); _volTimer = null; }
  _volAnalyser = null;
  const ind = document.getElementById('volume-indicator');
  if (ind) ind.style.display = 'none';
}

// ── Network quality ──────────────────────────────────────
let _statsTimer = null;

function startNetworkStats() {
  _statsTimer = setInterval(async () => {
    if (!peerConnection) return;
    try {
      const stats = await peerConnection.getStats();
      let lost = 0, recv = 0, rtt = null, pairType = null;
      const local = {}, remote = {};
      stats.forEach(r => {
        if (r.type === 'inbound-rtp' && r.kind === 'audio') {
          lost = r.packetsLost || 0; recv = r.packetsReceived || 1;
        }
        if (r.type === 'remote-inbound-rtp') rtt = r.roundTripTime;
        if (r.type === 'local-candidate') local[r.id] = r;
        if (r.type === 'remote-candidate') remote[r.id] = r;
      });
      // The selected candidate pair type is the key diagnostic: a call
      // riding on relay would not have connected without TURN.
      stats.forEach(r => {
        if ((r.type === 'candidate-pair') && (r.selected || r.nominated) && r.state === 'succeeded') {
          const lc = local[r.localCandidateId];
          if (lc) pairType = lc.candidateType;
        }
      });
      if (pairType) _lastPairType = pairType;
      const lossRate = recv > 0 ? (lost / (lost + recv)) * 100 : 0;

      // Adaptive bitrate: poor network → 24 kbps, fair → 32, good → 48.
      const target = lossRate >= 8 ? 24000 : lossRate >= 3 ? 32000 : 48000;
      if (target !== _curBitrate) { _curBitrate = target; _setSenderBitrate(target); }
      const qEl = document.getElementById('call-quality');
      const bEl = document.getElementById('quality-bars');
      const lEl = document.getElementById('quality-label');
      if (!qEl) return;
      qEl.style.display = 'flex';
      let grade, color, bars;
      if (lossRate < 2 && (!rtt || rtt < 0.15)) {
        grade = 'Good'; color = '#2ec27e'; bars = '▂▄▆';
      } else if (lossRate < 8 || (rtt && rtt < 0.3)) {
        grade = 'Fair'; color = '#e0b052'; bars = '▂▄▁';
      } else {
        grade = 'Poor'; color = '#e05252'; bars = '▂▁▁';
      }
      if (bEl) { bEl.textContent = bars; bEl.style.color = color; }
      if (lEl) {
        const via = _lastPairType === 'relay' ? ' · relay' : '';
        lEl.textContent = grade + (rtt ? ' · ' + Math.round(rtt * 1000) + 'ms' : '') + via;
        lEl.style.color = color;
      }
    } catch {}
  }, 3000);
}

function stopNetworkStats() {
  if (_statsTimer) { clearInterval(_statsTimer); _statsTimer = null; }
  const qEl = document.getElementById('call-quality');
  if (qEl) qEl.style.display = 'none';
}

// ── Video-call controls: hide on tap ─────────────────────────────────────────
// In a video call the buttons cover the other person, so a tap on the screen
// hides them and the next tap brings them back. The panel also leaves by
// itself after CONTROLS_IDLE_MS of inactivity, the way FaceTime and WhatsApp
// do it. In an audio call nothing is hidden: there is nothing to reveal, and
// vanishing buttons only alarm people.
const CONTROLS_IDLE_MS = 4000;
let _controlsTimer = null;

function _isVideoCallActive() {
  const ov = document.getElementById('call-overlay');
  return !!ov && ov.classList.contains('visible') && ov.classList.contains('video-mode');
}

function showCallControls(restartTimer) {
  const ov = document.getElementById('call-overlay');
  if (!ov) return;
  ov.classList.add('controls-visible');
  if (restartTimer !== false) _armControlsTimer();
}

function hideCallControls() {
  const ov = document.getElementById('call-overlay');
  if (!ov) return;
  ov.classList.remove('controls-visible');
  clearTimeout(_controlsTimer);
  _controlsTimer = null;
}

function _armControlsTimer() {
  clearTimeout(_controlsTimer);
  _controlsTimer = null;
  if (!_isVideoCallActive()) return;
  _controlsTimer = setTimeout(() => {
    // Never hidden while ringing: accept and decline are the only way to
    // answer.
    const active = document.getElementById('call-btns-active');
    if (active && active.style.display !== 'none') hideCallControls();
  }, CONTROLS_IDLE_MS);
}

// A tap on the call surface. A tap on the panel itself must not hide it, only
// extend its visibility, or missing a button would pull it out from under the
// finger. Taps on buttons never reach here: the delegator in events.js finds
// the nearest [data-act], which the button has of its own.
function callSurfaceTap(e) {
  if (!_isVideoCallActive()) return;
  if (e && e.target.closest('.call-box')) { _armControlsTimer(); return; }
  const ov = document.getElementById('call-overlay');
  if (ov.classList.contains('controls-visible')) hideCallControls();
  else showCallControls();
}

// ── Camera switch (front <-> rear) ───────────────────────────────────────────
let _facingMode = 'user';
let _flipInProgress = false;

// The button belongs only where there is something to switch to.
// enumerateDevices returns nothing before permission is granted, so it is
// called after getUserMedia.
async function updateFlipCameraAvailability() {
  const ov = document.getElementById('call-overlay');
  if (!ov) return;
  let multi = false;
  try {
    const devices = await navigator.mediaDevices.enumerateDevices();
    multi = devices.filter(d => d.kind === 'videoinput').length > 1;
  } catch {}
  ov.classList.toggle('has-multi-cam', multi);
}

async function flipCamera() {
  const btn = document.getElementById('flip-cam-btn');
  if (_flipInProgress || !localStream || !peerConnection) return;
  // While sharing a screen the sender holds the screen rather than a camera:
  // replaceTrack would swap the broadcast and break the way back.
  if (_screenStream) { toast('Stop screen sharing to switch the camera.'); return; }
  const oldTrack = localStream.getVideoTracks()[0];
  if (!oldTrack) return;

  _flipInProgress = true;
  if (btn) btn.disabled = true;
  const wasEnabled = oldTrack.enabled;
  const next = _facingMode === 'user' ? 'environment' : 'user';
  try {
    // iOS Safari will not hand over the second camera while the first is
    // busy, so the device is released before the new one is requested.
    oldTrack.stop();
    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: { exact: next }, width: { ideal: 1280 }, height: { ideal: 720 } },
        audio: false
      });
    } catch {
      // exact is unsupported (typically desktop) — fall back to a hint.
      stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: next, width: { ideal: 1280 }, height: { ideal: 720 } },
        audio: false
      });
    }
    const newTrack = stream.getVideoTracks()[0];
    if (!newTrack) throw new Error('no video track');
    newTrack.enabled = wasEnabled; // respect "camera off"

    const sender = peerConnection.getSenders().find(s => s.track && s.track.kind === 'video');
    if (sender) await sender.replaceTrack(newTrack);

    localStream.removeTrack(oldTrack);
    localStream.addTrack(newTrack);
    const lv = document.getElementById('local-video');
    if (lv) lv.srcObject = localStream;

    _facingMode = next;
    document.getElementById('call-overlay').classList.toggle('rear-camera', next === 'environment');
  } catch (err) {
    // The old track is already stopped, so without a rollback the user
    // would be left with a black window. Bring the previous camera back.
    console.warn('flipCamera failed:', err);
    try {
      const back = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: _facingMode, width: { ideal: 1280 }, height: { ideal: 720 } },
        audio: false
      });
      const t = back.getVideoTracks()[0];
      if (t) {
        t.enabled = wasEnabled;
        const sender = peerConnection && peerConnection.getSenders().find(s => s.track && s.track.kind === 'video');
        if (sender) await sender.replaceTrack(t);
        if (localStream) {
          try { localStream.removeTrack(oldTrack); } catch {}
          localStream.addTrack(t);
          const lv = document.getElementById('local-video');
          if (lv) lv.srcObject = localStream;
        }
      }
    } catch {}
    toast('Could not switch the camera.');
  } finally {
    _flipInProgress = false;
    if (btn) btn.disabled = false;
    _armControlsTimer();
  }
}

function toggleCamera() {
  if (!localStream) return;
  const vt = localStream.getVideoTracks()[0];
  if (!vt) return;
  vt.enabled = !vt.enabled;
  document.getElementById('camera-btn').classList.toggle('active', !vt.enabled);
  document.getElementById('camera-label').textContent = vt.enabled ? 'Camera' : 'Camera off';
  const lv = document.getElementById('local-video');
  if (lv) lv.style.opacity = vt.enabled ? '1' : '.25';
}

function setVolume(val) {
  document.getElementById('remote-audio').volume = val / 100;
  const rv = document.getElementById('remote-video');
  if (rv) rv.volume = val / 100;
}

// ── Sounds (Web Audio API) ───────────────────────────────
let ringtoneInterval = null;
let audioCtx = null;

function getAudioCtx() {
  if (!audioCtx || audioCtx.state === 'closed')
    audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  // iOS requires a resume after a user gesture
  if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
  return audioCtx;
}

function beep(freq, duration, gain, startTime) {
  const ctx = getAudioCtx();
  const osc = ctx.createOscillator();
  const vol = ctx.createGain();
  osc.connect(vol); vol.connect(ctx.destination);
  osc.frequency.value = freq; osc.type = 'sine';
  vol.gain.setValueAtTime(0, startTime);
  vol.gain.linearRampToValueAtTime(gain, startTime + 0.02);
  vol.gain.linearRampToValueAtTime(0, startTime + duration - 0.02);
  osc.start(startTime); osc.stop(startTime + duration);
}

function playRingtone() {
  stopRingtone();
  const ring = () => {
    const ctx = getAudioCtx(), t = ctx.currentTime;
    beep(480, 0.4, 0.15, t); beep(620, 0.4, 0.15, t + 0.05);
    beep(480, 0.4, 0.15, t + 0.5); beep(620, 0.4, 0.15, t + 0.55);
  };
  ring();
  ringtoneInterval = setInterval(ring, 3000);
}

function stopRingtone() {
  if (ringtoneInterval) { clearInterval(ringtoneInterval); ringtoneInterval = null; }
}

function playConnectSound() {
  const ctx = getAudioCtx(), t = ctx.currentTime;
  beep(880, 0.12, 0.2, t); beep(1100, 0.15, 0.2, t + 0.1);
}

// ── Lifecycle ─────────────────────────────────────────────
window.addEventListener('beforeunload', () => {
  if (callPeer && ws && ws.readyState === WebSocket.OPEN)
    ws.send(JSON.stringify({ type: 'call-end', to: callPeer }));
  stopRingtone();
});

document.addEventListener('visibilitychange', () => {
  // iOS Safari suspends the AudioContext in the background → resume on return
  if (document.visibilityState === 'visible' && audioCtx && audioCtx.state === 'suspended') {
    audioCtx.resume().catch(() => {});
  }
  if (document.visibilityState === 'visible' && ringtoneInterval && !pendingOffer) {
    stopRingtone();
  }
});

// ── Browser notifications ────────────────────────────────
function showNotification(from, body) {
  if (document.visibilityState === 'visible') return;
  // Muted conversation. The server sends no push for one at all, but this is
  // a different case: the tab is open in the background, the message arrives
  // over a live socket, and only the client can silence the notification.
  if (typeof chatIsMuted === 'function' && chatIsMuted(from)) return;
  if (!('Notification' in window) || Notification.permission !== 'granted') return;
  const text = body.startsWith('/files/') ? '📎 File' : body;
  const n = new Notification((window.APP_NAME || 'Hexeris') + ' — ' + from, {
    body: text.length > 60 ? text.substring(0, 60) + '…' : text,
    icon: '/LOGO_DARK.svg',
    tag: 'msg-' + from,
    renotify: true
  });
  n.onclick = () => { window.focus(); openChat(from); n.close(); };
  setTimeout(() => n.close(), 5000);
}
