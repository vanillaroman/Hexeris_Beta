// Hexeris — file uploads: queue, previews, drag-and-drop, paste.

// ── File upload ───────────────────────────────────────────────────────────────

// Allowed extensions — must stay in sync with server allowedExts map in main.go
const ALLOWED_EXTS = new Set([
  'jpg','jpeg','png','gif','webp','heic','heif',
  'mp4','mov','webm',
  'pdf','txt','zip','doc','docx','xls','xlsx'
]);
const MAX_FILE_SIZE = 50 * 1024 * 1024; // 50 MB — matches server ParseMultipartForm cap

// Simple sequential upload queue — if user pastes/drops multiple files at once
// they upload one-by-one instead of racing over a single progress bar.
const uploadQueue = [];
let uploading = false;

async function drainUploadQueue() {
  if (uploading || !uploadQueue.length) return;
  uploading = true;
  while (uploadQueue.length) {
    const file = uploadQueue.shift();
    await sendFileObject(file);
  }
  uploading = false;
}

// ── File preview (confirm before send) ───────────────────────────────────────

let _previewResolve = null;

function enqueueFile(file) {
  if (!activePeer) return;
  const normalized = normalizeClipboardFile(file);
  if (!validateFile(normalized)) return;
  showFilePreview(normalized);
}

function showFilePreview(file) {
  const modal   = document.getElementById('file-preview-modal');
  const thumb   = document.getElementById('fp-thumb');
  const icon    = document.getElementById('fp-icon');
  const name    = document.getElementById('fp-name');
  const size    = document.getElementById('fp-size');

  name.textContent = file.name;
  size.textContent = file.size < 1024 * 1024
    ? (file.size / 1024).toFixed(1) + ' KB'
    : (file.size / 1024 / 1024).toFixed(2) + ' MB';

  if (file.type.startsWith('image/')) {
    const url = URL.createObjectURL(file);
    thumb.src = url;
    thumb.style.display = 'block';
    icon.style.display  = 'none';
    // Release the blob URL on a decode error too (a corrupt file), or every
    // failed preview would hold the file in memory forever.
    thumb.onload = thumb.onerror = () => URL.revokeObjectURL(url);
  } else {
    thumb.style.display = 'none';
    icon.style.display  = 'block';
    // pick icon char based on type
    const ext = file.name.split('.').pop().toLowerCase();
    icon.textContent = /mp4|mov|webm/.test(ext) ? '🎬'
                     : /pdf/.test(ext)           ? '📄'
                     : /zip/.test(ext)           ? '🗜'
                     : /xls|xlsx/.test(ext)      ? '📊'
                     : /doc|docx/.test(ext)      ? '📝' : '📎';
  }

  modal.classList.add('active');
  document.getElementById('fp-confirm').focus();

  // Return a Promise so the modal can be await-ed in future if needed;
  // currently we resolve it immediately inside the handlers below.
  _previewResolve = (confirmed) => {
    modal.classList.remove('active');
    thumb.src = '';
    if (confirmed) {
      uploadQueue.push(file);
      drainUploadQueue();
    }
    _previewResolve = null;
  };
}

function confirmFilePreview() { _previewResolve && _previewResolve(true); }
function cancelFilePreview()  { _previewResolve && _previewResolve(false); }

// Clipboard blobs come as File objects with name="blob" and no extension.
// Derive a proper name+extension from the MIME type so the server can
// validate the extension and produce the right media_type.
function normalizeClipboardFile(file) {
  if (file.name && file.name !== 'blob' && file.name.includes('.')) return file;
  const mimeToExt = {
    'image/png': 'png', 'image/jpeg': 'jpg', 'image/gif': 'gif',
    'image/webp': 'webp', 'video/mp4': 'mp4', 'video/webm': 'webm',
    'application/pdf': 'pdf', 'text/plain': 'txt',
  };
  const ext = mimeToExt[file.type] || file.type.split('/')[1] || 'bin';
  return new File([file], `paste-${Date.now()}.${ext}`, { type: file.type });
}

function validateFile(file) {
  // Any file type is allowed. The server accepts everything: known types are
  // shown inline (raster images and video only) and the rest is served as a
  // forced download rather than rendered in this origin. Only the size limit
  // remains.
  if (file.size > MAX_FILE_SIZE) {
    showFileError(`File too large (${(file.size/1024/1024).toFixed(1)} MB). Max 50 MB.`);
    return false;
  }
  return true;
}

function showFileError(msg) {
  // Reuse the progress bar area as a non-blocking error toast
  const el = document.getElementById('upload-progress');
  const label = document.getElementById('upload-progress-label');
  const fill = document.getElementById('upload-progress-fill');
  label.textContent = '⚠ ' + msg.split('\n')[0];
  fill.style.width = '0%';
  fill.style.background = '#e03c3c';
  el.classList.add('visible');
  setTimeout(() => {
    el.classList.remove('visible');
    fill.style.background = '';
  }, 3500);
}

async function sendFileObject(file) {
  if (!activePeer) return;

  const progressEl = document.getElementById('upload-progress');
  const progressFill = document.getElementById('upload-progress-fill');
  const progressLabel = document.getElementById('upload-progress-label');
  progressFill.style.background = '';
  progressEl.classList.add('visible');
  progressFill.style.width = '0%';
  progressLabel.textContent = `Uploading ${file.name}…`;

  const formData = new FormData();
  formData.append('file', file);

  try {
    const data = await new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open('POST', `${location.protocol}//${SERVER}/upload`);
      xhr.setRequestHeader('Authorization', 'Bearer ' + token);
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) {
          const pct = Math.round(e.loaded / e.total * 100);
          progressFill.style.width = pct + '%';
          progressLabel.textContent = `Uploading ${file.name}… ${pct}%`;
        }
      };
      xhr.onload = () => {
        if (xhr.status === 200) { resolve(JSON.parse(xhr.responseText)); return; }
        // A 413 usually comes from the reverse proxy and its body is an HTML
        // page, which is useless as an error message. Show something clear.
        if (xhr.status === 413) { reject(new Error('File too large (server limit)')); return; }
        const t = (xhr.responseText || '').trim();
        const msg = (!t || /^</.test(t)) ? `Upload error (${xhr.status})` : t;
        reject(new Error(msg));
      };
      xhr.onerror = () => reject(new Error('Network error'));
      xhr.send(formData);
    });

    progressFill.style.width = '100%';
    setTimeout(() => progressEl.classList.remove('visible'), 500);

    const msgID = Date.now() + '-' + Math.floor(Math.random() * 99999);
    // The real file name goes into the body's #fragment: the browser never
    // sends it to the server, while the recipient sees and saves the real name.
    const fileBody = data.url + '#' + encodeURIComponent(file.name);
    // queueMessage: the file is already uploaded, and the link message goes
    // through the persistent queue so it survives a reconnect.
    queueMessage({
      type: 'message', id: msgID, from: myUsername, to: activePeer,
      body: fileBody, media_type: data.media_type
    });
    addToChat(activePeer, {
      id: msgID, from: myUsername, to: activePeer,
      body: fileBody, media_type: data.media_type, status: 'sending', ts: Date.now()
    });
    renderMessages(activePeer);
    const preview = data.media_type === 'image' ? '🖼 Image'
                  : data.media_type === 'video' ? '🎬 Video' : '📎 File';
    updateContact(activePeer, preview);
  } catch(e) {
    progressEl.classList.remove('visible');
    showFileError('Upload failed: ' + e.message);
  }
}

// Legacy entry point from <input type=file onchange=sendFile(this)>
function sendFile(input) {
  if (!input.files[0]) return;
  const file = input.files[0];
  input.value = '';
  enqueueFile(file);
}

// ── Paste & Drag-drop init ────────────────────────────────────────────────────

function initFileInput() {
  // ── Preview modal keyboard shortcuts ─────────────────────────────────────
  document.addEventListener('keydown', (e) => {
    if (!_previewResolve) return;
    if (e.key === 'Escape') { e.preventDefault(); cancelFilePreview(); }
    if (e.key === 'Enter')  { e.preventDefault(); confirmFilePreview(); }
  });

  // ── Clipboard paste ──────────────────────────────────────────────────────
  // Intercept paste only when the clipboard contains files/images.
  // If it's plain text, we let the textarea handle it as normal typing.
  document.addEventListener('paste', (e) => {
    if (!activePeer) return;
    const items = [...(e.clipboardData?.items || [])].filter(i => i.kind === 'file');
    if (!items.length) return;   // no files → let textarea handle text paste
    e.preventDefault();
    items.forEach(item => {
      const file = item.getAsFile();
      if (file) enqueueFile(file);
    });
  });

  // ── Drag & drop ──────────────────────────────────────────────────────────
  // We attach to chat-main so the overlay only activates inside the active chat.
  const zone = document.getElementById('chat-main');
  const overlay = document.getElementById('drop-overlay');
  if (!zone || !overlay) return;

  // dragCounter prevents premature hide when cursor moves over child elements
  // (each child fires dragleave/dragenter as the cursor crosses its border).
  let dragCounter = 0;

  zone.addEventListener('dragenter', (e) => {
    if (!activePeer) return;
    if (![...e.dataTransfer.types].includes('Files')) return;
    e.preventDefault();
    if (++dragCounter === 1) overlay.classList.add('active');
  });

  zone.addEventListener('dragleave', () => {
    if (--dragCounter <= 0) {
      dragCounter = 0;
      overlay.classList.remove('active');
    }
  });

  zone.addEventListener('dragover', (e) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  });

  zone.addEventListener('drop', (e) => {
    e.preventDefault();
    dragCounter = 0;
    overlay.classList.remove('active');
    if (!activePeer) return;
    [...e.dataTransfer.files].forEach(f => enqueueFile(f));
  });
}

async function deleteMessage(msgID) {
  if (!await hexConfirm('Delete this message for everyone?', { okText: 'Delete', danger: true })) return;
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/delete-message`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token },
      body: JSON.stringify({ msg_id: msgID })
    });
    if (!resp.ok) { toast('Delete failed'); return; }
    // Mark it deleted locally
    for (const peer of Object.keys(chats)) {
      const m = chats[peer].find(m => m.id === msgID);
      if (m) {
        m.deleted = true; m.body = '[deleted]';
        if (activePeer === peer) renderMessages(peer);
        break;
      }
    }
  } catch(e) { toast('Error: ' + e.message); }
}

async function sendMessage() {
  if (!activePeer) return;
  const ta = document.getElementById('msg-textarea');
  let text = ta.value.trim();
  if (!text) return;

  // Edit mode: update the existing message instead of sending a new one.
  if (editingMsg) { submitEdit(activePeer, editingMsg, text); return; }

  // "Send as code" mode (the </> button in the composer). We wrap the text in
  // ``` instead of adding a separate flag in the database: the result is plain
  // markdown that any other client understands and that stays readable in an
  // export. Already wrapped text is not wrapped again — nested ``` break
  // parsing and put stray quotes on screen.
  if (typeof codeModeOn === 'function' && codeModeOn() && !/^```[\s\S]*```$/.test(text)) {
    text = '```\n' + text + '\n```';
  }
  if (typeof setCodeMode === 'function') setCodeMode(false);

  ta.value = '';
  ta.style.height = 'auto';
  if (typeof updateComposerState === 'function') updateComposerState();

  // Sending does not depend on the socket: the message enters the persistent
  // queue and is delivered on the next connection.
  const msgID = Date.now() + '-' + Math.floor(Math.random() * 99999);
  queueMessage({ type: 'message', id: msgID, from: myUsername, to: activePeer,
                 body: text, reply_to: replyToMsg || undefined });

  addToChat(activePeer, { id: msgID, from: myUsername, to: activePeer, body: text, status: 'sending', ts: Date.now(), reply_to: replyToMsg || null });
  cancelReply();
  renderMessages(activePeer);
  updateContact(activePeer, text);
  updateOfflineBanner();
}
