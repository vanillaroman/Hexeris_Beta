// ── Groups ────────────────────────────────────────────────
// Groups live on the server (the groups and group_members tables) while their
// messages travel the ordinary stream with recipient="g:<id>". This module
// loads the list, hosts the creation modal and the member panel with its
// admin actions, and reacts to the server's live group-changed event.

let groups = {}; // gid -> {id, name, members: {username: role}}

function isGroupId(id) { return typeof id === 'string' && id.startsWith('g:'); }

function groupDisplayName(gid) {
  return (groups[gid] && groups[gid].name) || 'Group';
}

function myGroupRole(gid) {
  return (groups[gid] && groups[gid].members && groups[gid].members[myUsername]) || null;
}

async function loadGroups() {
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/groups`, {
      headers: { 'Authorization': `Bearer ${token}` }
    });
    if (!resp.ok) return;
    const list = await resp.json();
    const fresh = {};
    const newGids = [];
    for (const g of list) {
      fresh[g.id] = g;
      if (!groups[g.id]) newGids.push(g.id);
      if (!chats[g.id]) chats[g.id] = []; // a group is listed even with no messages
    }
    // Drop the conversation and cache of any group we are no longer in.
    // Iterating chats rather than groups matters: after a reload groups is
    // empty while abandoned groups remain in local storage and reappear as
    // ghosts with no members.
    for (const gid of Object.keys(chats)) {
      if (!gid.startsWith('g:') || fresh[gid]) continue;
      delete chats[gid];
      try {
        localStorage.removeItem(`hc_msgs_${myUsername}_${gid}`);
      } catch (e) {}
      if (activePeer === gid) closeActiveGroupChat();
    }
    groups = fresh;
    // The main sync will not bring old messages of a group we just joined
    // (they sit below the global seq cursor), so its history is pulled here.
    for (const gid of newGids) {
      if (typeof loadPeerHistory === 'function' && !(chats[gid] || []).length) loadPeerHistory(gid);
    }
    renderContacts();
    if (activePeer && isGroupId(activePeer)) showChatHeader(activePeer);
  } catch {}
}

function closeActiveGroupChat() {
  activePeer = null;
  saveActivePeer('');
  document.getElementById('chat-main').style.display = 'none';
  document.getElementById('chat-empty').style.display = '';
  closeGroupPanel();
  // On mobile show the sidebar rather than an empty chat area
  if (typeof showSidebar === 'function') showSidebar();
  renderContacts();
}

// The server sent group-changed (creation, membership, roles) — reload.
function handleGroupChanged() { loadGroups(); }

// ── Creation ──────────────────────────────────────────────

function openGroupModal() {
  document.getElementById('group-modal').style.display = 'flex';
  document.getElementById('group-name-input').value = '';
  document.getElementById('group-members-input').value = '';
  document.getElementById('group-modal-error').textContent = '';
  document.getElementById('group-name-input').focus();
}

function closeGroupModal() {
  document.getElementById('group-modal').style.display = 'none';
}

async function createGroupSubmit() {
  const name = document.getElementById('group-name-input').value.trim();
  const members = document.getElementById('group-members-input').value
    .split(/[,\s]+/).map(s => s.trim()).filter(Boolean);
  const errEl = document.getElementById('group-modal-error');
  if (!name) { errEl.textContent = 'Enter a group name'; return; }
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/groups`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` },
      body: JSON.stringify({ name, members })
    });
    if (!resp.ok) { errEl.textContent = 'Failed to create group'; return; }
    const g = await resp.json();
    groups[g.id] = g;
    if (!chats[g.id]) chats[g.id] = [];
    closeGroupModal();
    renderContacts();
    openChat(g.id);
  } catch {
    errEl.textContent = 'Network error, please try again';
  }
}

// ── Member panel ──────────────────────────────────────────

function openGroupPanel() {
  if (!activePeer || !isGroupId(activePeer)) return;
  renderGroupPanel(activePeer);
  document.getElementById('group-panel').style.display = 'flex';
}

function closeGroupPanel() {
  const p = document.getElementById('group-panel');
  if (p) p.style.display = 'none';
}

function renderGroupPanel(gid) {
  const g = groups[gid];
  const box = document.getElementById('group-panel-box');
  if (!g || !box) return;
  const iAmAdmin = myGroupRole(gid) === 'admin';
  const names = Object.keys(g.members).sort((a, b) => {
    // admins first, then alphabetically
    const ra = g.members[a] === 'admin' ? 0 : 1;
    const rb = g.members[b] === 'admin' ? 0 : 1;
    return ra - rb || a.localeCompare(b);
  });

  let rows = '';
  for (const u of names) {
    const role = g.members[u];
    let actions = '';
    if (iAmAdmin && u !== myUsername) {
      actions =
        `<button class="gp-act" data-act="setGroupRole" data-a1="${escHtml(u)}" data-a2="${role === 'admin' ? 'member' : 'admin'}">` +
          (role === 'admin' ? 'Remove admin' : 'Make admin') + `</button>` +
        `<button class="gp-act gp-danger" data-act="removeGroupMember" data-a1="${escHtml(u)}">Remove</button>`;
    }
    rows += `
      <div class="gp-member">
        <div class="contact-avatar ${avatarClass(u)}">${escHtml(u[0].toUpperCase())}</div>
        <div class="gp-member-name">${escHtml(u)}${role === 'admin' ? ' <span class="gp-role">admin</span>' : ''}</div>
        <div class="gp-actions">${actions}</div>
      </div>`;
  }

  const addBlock = iAmAdmin ? `
    <div class="gp-add">
      <input id="gp-add-input" class="new-chat-input" placeholder="Add member..."
             data-act-enter="addGroupMember"/>
      <button class="new-chat-btn" data-act="addGroupMember" title="Add" aria-label="Add member">
        <svg viewBox="0 0 24 24"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
      </button>
    </div>` : '';

  // Only an admin edits the name and purpose. Everyone else sees plain text:
  // an input that saves nothing is worse than no input.
  const titleBlock = iAmAdmin
    ? `<input class="gp-name-input" id="gp-name" value="${escHtml(g.name)}" maxlength="64"
              aria-label="Group name" data-act-enter="saveGroupName"/>`
    : `<div class="gp-title">${escHtml(g.name)}</div>`;

  const descr = g.description || '';
  const descrBlock = iAmAdmin
    ? `<textarea class="gp-descr-input" id="gp-descr" rows="2" maxlength="280"
                 aria-label="Group description"
                 placeholder="What is this group for? (optional)">${escHtml(descr)}</textarea>
       <div class="gp-save-row">
         <span class="gp-hint" id="gp-hint"></span>
         <button class="gp-save" data-act="saveGroupInfo">Save</button>
       </div>`
    : (descr ? `<div class="gp-descr">${escHtml(descr)}</div>` : '');

  const dangerBlock = iAmAdmin
    ? `<button class="gp-leave gp-danger-btn" data-act="deleteGroup">Delete group</button>`
    : '';

  box.innerHTML = `
    <div class="gp-header">
      ${titleBlock}
      <button class="gp-close" data-act="closeGroupPanel">✕</button>
    </div>
    <div class="gp-sub">${names.length} member${names.length === 1 ? '' : 's'}</div>
    ${descrBlock}
    <div class="gp-members">${rows}</div>
    ${addBlock}
    <button class="gp-leave" data-act="leaveGroup">Leave group</button>
    ${dangerBlock}`;
}

async function _groupApi(path, body) {
  try {
    const resp = await fetch(`${location.protocol}//${SERVER}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` },
      body: JSON.stringify(body)
    });
    return resp.ok;
  } catch { return false; }
}

async function addGroupMember() {
  const inp = document.getElementById('gp-add-input');
  const name = inp ? inp.value.trim() : '';
  if (!name || !activePeer) return;
  if (await _groupApi('/groups/members', { group_id: activePeer, add: [name] })) {
    await loadGroups();
    renderGroupPanel(activePeer);
  } else if (inp) {
    inp.value = '';
    inp.placeholder = 'Could not add user';
  }
}

async function removeGroupMember(user) {
  if (!activePeer) return;
  if (await _groupApi('/groups/members', { group_id: activePeer, remove: [user] })) {
    await loadGroups();
    if (activePeer) renderGroupPanel(activePeer);
  }
}

async function setGroupRole(user, role) {
  if (!activePeer) return;
  if (await _groupApi('/groups/role', { group_id: activePeer, username: user, role })) {
    await loadGroups();
    if (activePeer) renderGroupPanel(activePeer);
  }
}

// Name and description are saved in one request: the user edits both fields
// and presses Save once. The server accepts a partial update, but two
// requests for one button would be pointless.
async function saveGroupInfo() {
  const gid = activePeer;
  if (!isGroupId(gid)) return;
  const nameEl  = document.getElementById('gp-name');
  const descrEl = document.getElementById('gp-descr');
  const hint    = document.getElementById('gp-hint');
  if (!nameEl) return;

  const name = nameEl.value.trim();
  if (!name) { if (hint) { hint.textContent = 'Name cannot be empty'; hint.className = 'gp-hint err'; } return; }

  if (hint) { hint.textContent = 'Saving…'; hint.className = 'gp-hint'; }
  const body = { group_id: gid, name };
  if (descrEl) body.description = descrEl.value;

  try {
    const resp = await fetch(`${location.protocol}//${SERVER}/groups/update`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` },
      body: JSON.stringify(body)
    });
    if (!resp.ok) {
      const t = (await resp.text()).trim();
      if (hint) { hint.textContent = t || 'Could not save'; hint.className = 'gp-hint err'; }
      return;
    }
    const g = await resp.json();
    // Updated locally straight away: group-changed will arrive anyway, but
    // until it does the chat header and contact list show the old name.
    if (groups[gid]) { groups[gid].name = g.name; groups[gid].description = g.description; }
    if (hint) { hint.textContent = 'Saved'; hint.className = 'gp-hint ok'; }
    if (typeof renderContacts === 'function') renderContacts();
    if (typeof showChatHeader === 'function') showChatHeader(gid);
  } catch {
    if (hint) { hint.textContent = 'Cannot reach the server'; hint.className = 'gp-hint err'; }
  }
}

// Enter in the name field saves without reaching for the button.
function saveGroupName() { saveGroupInfo(); }

async function deleteGroup() {
  const gid = activePeer;
  if (!isGroupId(gid)) return;
  const name = (groups[gid] && groups[gid].name) || 'this group';
  // Disbanding is irreversible for the membership, so confirm.
  const ok = await hexConfirm(
    `Delete “${name}”? The group and its member list disappear for everyone. Messages stay in the archive.`,
    { title: 'Delete group', okText: 'Delete', danger: true });
  if (!ok) return;
  const done = await _groupApi('/groups/delete', { group_id: gid });
  if (done) {
    closeGroupPanel();
    closeActiveGroupChat();
    loadGroups();
  }
}

async function leaveGroup() {
  if (!activePeer) return;
  const gid = activePeer;
  if (!await hexConfirm(`Leave group "${groupDisplayName(gid)}"?`, { okText: 'Leave', danger: true })) return;
  if (await _groupApi('/groups/leave', { group_id: gid })) {
    delete groups[gid];
    delete chats[gid];
    closeActiveGroupChat();
    renderContacts();
  }
}
