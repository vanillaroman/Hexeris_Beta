// Hexeris — voice messages: record (MediaRecorder), waveform, playback.
//
// Recording reuses the existing encrypted /upload endpoint: the audio blob is
// uploaded as a normal file (.webm on Chrome/Firefox, .m4a on Safari — both
// already allowed server-side) and then a chat message with media_type:"voice"
// is queued through the reliable outbox (queueMessage). The server stores the
// client-supplied media_type verbatim, so no protocol/DB change is needed.
//
// Duration is carried in the URL fragment (#d=<seconds>) — fragments are never
// sent to the server on the follow-up GET, so it stays a purely client-side hint
// and needs no new column.

let _mediaRecorder = null;
let _voiceChunks = [];
let _voiceStream = null;
let _voiceStart = 0;
let _voiceTimer = null;
let _voiceMime = '';

// Pick the best-supported audio container. Safari only does audio/mp4.
function _pickVoiceMime() {
  const candidates = ['audio/webm;codecs=opus', 'audio/webm', 'audio/mp4', 'audio/aac'];
  if (typeof MediaRecorder === 'undefined' || !MediaRecorder.isTypeSupported) return '';
  for (const c of candidates) { if (MediaRecorder.isTypeSupported(c)) return c; }
  return '';
}

function _voiceExt(mime) {
  if (mime.includes('webm')) return 'webm';
  if (mime.includes('mp4') || mime.includes('aac')) return 'm4a';
  return 'webm';
}

async function startVoiceRecording() {
  if (!activePeer) return;
  if (_mediaRecorder) return; // already recording
  if (typeof MediaRecorder === 'undefined' || !navigator.mediaDevices?.getUserMedia) {
    toast('Voice messages are not supported in this browser'); return;
  }
  try {
    _voiceStream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch {
    toast('Microphone access denied'); return;
  }
  _voiceMime = _pickVoiceMime();
  try {
    _mediaRecorder = _voiceMime ? new MediaRecorder(_voiceStream, { mimeType: _voiceMime })
                                : new MediaRecorder(_voiceStream);
  } catch {
    _mediaRecorder = new MediaRecorder(_voiceStream);
  }
  _voiceMime = _mediaRecorder.mimeType || _voiceMime || 'audio/webm';
  _voiceChunks = [];
  _mediaRecorder.ondataavailable = (e) => { if (e.data && e.data.size) _voiceChunks.push(e.data); };
  _mediaRecorder.start();
  _voiceStart = Date.now();

  const ia = document.querySelector('.input-area'); if (ia) ia.style.display = 'none';
  document.getElementById('voice-rec-bar').classList.add('active');
  _renderLiveWave();
  _voiceTimer = setInterval(() => {
    const s = Math.floor((Date.now() - _voiceStart) / 1000);
    document.getElementById('voice-rec-time').textContent = _fmtDur(s);
    // Hard cap at 5 minutes so a stuck recorder can't fill the disk.
    if (s >= 300) stopAndSendVoice();
  }, 200);
}

function _cleanupRecorder() {
  clearInterval(_voiceTimer); _voiceTimer = null;
  if (_voiceStream) { _voiceStream.getTracks().forEach(t => t.stop()); _voiceStream = null; }
  document.getElementById('voice-rec-bar').classList.remove('active');
  const ia = document.querySelector('.input-area'); if (ia) ia.style.display = '';
  document.getElementById('voice-rec-time').textContent = '0:00';
  document.getElementById('voice-rec-wave').innerHTML = '';
}

function cancelVoiceRecording() {
  if (!_mediaRecorder) return;
  const rec = _mediaRecorder; _mediaRecorder = null;
  rec.onstop = () => {}; // discard
  try { rec.stop(); } catch {}
  _voiceChunks = [];
  _cleanupRecorder();
}

function stopAndSendVoice() {
  if (!_mediaRecorder) return;
  const rec = _mediaRecorder; _mediaRecorder = null;
  const durSec = Math.max(1, Math.round((Date.now() - _voiceStart) / 1000));
  rec.onstop = async () => {
    const blob = new Blob(_voiceChunks, { type: _voiceMime });
    _voiceChunks = [];
    _cleanupRecorder();
    if (blob.size < 512) { toast('Recording too short'); return; }
    await _uploadAndSendVoice(blob, durSec);
  };
  try { rec.stop(); } catch { _cleanupRecorder(); }
}

async function _uploadAndSendVoice(blob, durSec) {
  const peer = activePeer;
  const ext = _voiceExt(_voiceMime);
  const file = new File([blob], `voice-${Date.now()}.${ext}`, { type: blob.type });

  const progressEl = document.getElementById('upload-progress');
  const progressFill = document.getElementById('upload-progress-fill');
  const progressLabel = document.getElementById('upload-progress-label');
  progressFill.style.background = '';
  progressEl.classList.add('visible');
  progressFill.style.width = '0%';
  progressLabel.textContent = 'Sending voice message…';

  const fd = new FormData();
  fd.append('file', file);
  try {
    const data = await new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('POST', `${location.protocol}//${SERVER}/upload`);
      xhr.setRequestHeader('Authorization', 'Bearer ' + token);
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) progressFill.style.width = Math.round(e.loaded / e.total * 100) + '%';
      };
      xhr.onload = () => xhr.status === 200 ? resolve(JSON.parse(xhr.responseText)) : reject(new Error(xhr.responseText || xhr.statusText));
      xhr.onerror = () => reject(new Error('Network error'));
      xhr.send(fd);
    });
    progressFill.style.width = '100%';
    setTimeout(() => progressEl.classList.remove('visible'), 400);

    const url = data.url + '#d=' + durSec;
    const msgID = Date.now() + '-' + Math.floor(Math.random() * 99999);
    queueMessage({ type: 'message', id: msgID, from: myUsername, to: peer, body: url, media_type: 'voice' });
    addToChat(peer, { id: msgID, from: myUsername, to: peer, body: url, media_type: 'voice', status: 'sending', ts: Date.now() });
    if (activePeer === peer) renderMessages(peer);
    updateContact(peer, '🎤 Voice message');
    updateOfflineBanner();
  } catch (e) {
    progressEl.classList.remove('visible');
    toast('Voice message failed: ' + e.message);
  }
}

function _fmtDur(totalSec) {
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return m + ':' + String(s).padStart(2, '0');
}

// Live "listening" bars while recording (cosmetic, cheap — no PCM analysis).
function _renderLiveWave() {
  const wave = document.getElementById('voice-rec-wave');
  if (!wave) return;
  wave.innerHTML = Array.from({ length: 28 }, () => '<span></span>').join('');
}

// ── Playback ──────────────────────────────────────────────────────────────────
// A single shared <audio> plays at most one voice message at a time. Progress is
// painted onto the active bubble's waveform; a re-render (renderMessages rebuilds
// innerHTML) doesn't stop audio — the next timeupdate re-resolves the element.
let _voiceAudio = null;
let _voicePlayingId = null;
let _voiceRate = 1;

// 28 pseudo-random bar heights, deterministic from the message id, so the same
// message always shows the same waveform on every device (no stored peak data).
function voiceWaveBars(id) {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  const bars = [];
  for (let i = 0; i < 28; i++) {
    h = (h * 1103515245 + 12345) >>> 0;
    bars.push(18 + (h % 82)); // 18%..100%
  }
  return bars;
}

function voiceBubbleHtml(m) {
  const dur = _voiceDurFromBody(m.body);
  const bars = voiceWaveBars(m.id).map((pct, i) =>
    `<span style="height:${pct}%" data-i="${i}"></span>`).join('');
  const playing = _voicePlayingId === m.id;
  return `<div class="voice-msg" data-vid="${escHtml(m.id)}">
    <button class="voice-play" data-act="toggleVoice" data-a1="${escHtml(m.id)}" data-a2="${escHtml(m.body)}">${playing ? _pauseIcon : _playIcon}</button>
    <div class="voice-wave" id="vw-${escHtml(m.id)}">${bars}</div>
    <span class="voice-dur" id="vd-${escHtml(m.id)}">${_fmtDur(dur)}</span>
    <button class="voice-rate" data-act="cycleVoiceRate">${_voiceRate}×</button>
  </div>`;
}

const _playIcon = '<svg viewBox="0 0 24 24"><polygon points="6 4 20 12 6 20 6 4"/></svg>';
const _pauseIcon = '<svg viewBox="0 0 24 24"><rect x="6" y="5" width="4" height="14"/><rect x="14" y="5" width="4" height="14"/></svg>';

function _voiceDurFromBody(body) {
  const m = /#d=(\d+(?:\.\d+)?)/.exec(body || '');
  return m ? Math.round(parseFloat(m[1])) : 0;
}
function _voiceSrc(body) { return (body || '').split('#')[0]; }

function toggleVoice(id, body) {
  if (_voicePlayingId === id && _voiceAudio && !_voiceAudio.paused) {
    _voiceAudio.pause();
    return;
  }
  if (_voiceAudio) { try { _voiceAudio.pause(); } catch {} }
  _voiceAudio = new Audio(_voiceSrc(body));
  _voiceAudio.playbackRate = _voiceRate;
  _voicePlayingId = id;

  _voiceAudio.ontimeupdate = () => _paintVoiceProgress(id);
  _voiceAudio.onended = () => { _resetVoiceUI(id); _voicePlayingId = null; };
  _voiceAudio.onpause = () => _syncVoiceButton(id, false);
  _voiceAudio.onplay  = () => _syncVoiceButton(id, true);
  _voiceAudio.play().catch(() => toast('Cannot play audio'));
}

function _paintVoiceProgress(id) {
  const wave = document.getElementById('vw-' + id);
  const dEl = document.getElementById('vd-' + id);
  if (!_voiceAudio) return;
  const pct = _voiceAudio.duration ? _voiceAudio.currentTime / _voiceAudio.duration : 0;
  if (wave) {
    const bars = wave.children;
    const filled = Math.floor(pct * bars.length);
    for (let i = 0; i < bars.length; i++) bars[i].classList.toggle('on', i <= filled);
  }
  if (dEl) {
    const remain = _voiceAudio.duration ? Math.round(_voiceAudio.duration - _voiceAudio.currentTime) : 0;
    dEl.textContent = _fmtDur(remain);
  }
}

function _resetVoiceUI(id) {
  const wave = document.getElementById('vw-' + id);
  if (wave) for (const b of wave.children) b.classList.remove('on');
  _syncVoiceButton(id, false);
}

function _syncVoiceButton(id, playing) {
  const box = document.querySelector(`.voice-msg[data-vid="${id}"] .voice-play`);
  if (box) box.innerHTML = playing ? _pauseIcon : _playIcon;
}

function cycleVoiceRate(btn) {
  _voiceRate = _voiceRate === 1 ? 1.5 : _voiceRate === 1.5 ? 2 : 1;
  btn.textContent = _voiceRate + '×';
  if (_voiceAudio) _voiceAudio.playbackRate = _voiceRate;
}
