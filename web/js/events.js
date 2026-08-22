// Hexeris — central event delegation.
//
// Interactivity used to hang on inline attributes (onclick="..."), which
// forced the Content-Security-Policy to allow script-src 'unsafe-inline' and
// so gave up its main purpose: under any XSS an injected <script> or onerror=
// would run as first-party code.
//
// The markup now only declares intent:
//
//   <button data-act="sendMessage">               — no arguments
//   <button data-act="switchTab" data-a1="login"> — arguments in data-a1/a2
//
// while the mapping from name to function lives here in ACTIONS. The registry
// is explicit rather than window[name] on purpose: the DOM must not be able
// to call an arbitrary global by a string in an attribute.
//
// Listeners are attached once to the document and cover elements created
// later (the message feed, the group panel), so nothing needs re-binding
// after a render.

(function () {
  // el is the element carrying data-act, e is the original event.
  const ACTIONS = {
    // ── Auth ──
    doAuth:            () => doAuth(),
    switchTab:         (e, el) => switchTab(el.dataset.a1),
    togglePasswordVis: (e, el) => {
      const i = document.getElementById('auth-password');
      i.type = i.type === 'password' ? 'text' : 'password';
    },
    submitPasswordChange: () => submitPasswordChange(),
    reloadPage: () => location.reload(),

    // ── Chats and sidebar ──
    openChat:           () => openChat(),
    showSidebar:        () => showSidebar(),
    openGroupModal:     () => openGroupModal(),
    createGroupSubmit:  () => createGroupSubmit(),
    closeGroupModal:    () => closeGroupModal(),
    openGroupPanel:     () => openGroupPanel(),
    closeGroupPanel:    () => closeGroupPanel(),
    addGroupMember:     () => addGroupMember(),
    removeGroupMember:  (e, el) => removeGroupMember(el.dataset.a1),
    setGroupRole:       (e, el) => setGroupRole(el.dataset.a1, el.dataset.a2),
    leaveGroup:         () => leaveGroup(),
    saveGroupInfo:      () => saveGroupInfo(),
    saveGroupName:      () => saveGroupName(),
    deleteGroup:        () => deleteGroup(),
    logout:             () => { closeSettingsMenu(); logout(); },

    // ── Profile and settings ──
    openMyProfile:      () => openMyProfile(),
    closeMyProfile:     () => closeMyProfile(),
    saveMyProfile:      () => saveMyProfile(),
    viewPeerProfile:    () => openPeerProfile(activePeer),
    closePeerProfile:   () => closePeerProfile(),
    pickPresence:       (e, el) => pickPresence(el.dataset.a1),
    pickAvatarFile:     () => document.getElementById('pf-avatar-file').click(),
    uploadAvatar:       (e, el) => uploadAvatar(el),
    toggleSettingsMenu: (e) => toggleSettingsMenu(e),
    // The settings menu items live inside it, so the outside-click handler
    // does not close them; they are closed explicitly.
    toggleNotifications: () => { closeSettingsMenu(); toggleNotifications(); },

    // ── Search ──
    onSearchInput:     () => onSearchInput(),
    toggleChatSearch:  () => toggleChatSearch(),
    closeChatSearch:   () => closeChatSearch(),
    onChatSearchInput: () => onChatSearchInput(),
    chatSearchStep:    (e, el) => chatSearchStep(Number(el.dataset.a1)),
    // Enter goes to the next match, Shift+Enter to the previous one. Escape
    // closes the search through the global Escape handler.
    chatSearchEnter:   (e) => chatSearchStep(e.shiftKey ? -1 : 1),

    // ── Composer ──
    sendMessage:        () => sendMessage(),
    composerInput:      (e, el) => { autoResize(el); sendTyping(); },
    toggleAttachMenu:   (e) => toggleAttachMenu(e),
    pickAttach:         (e, el) => pickAttach(el.dataset.a1),
    filesPicked:        (e, el) => { [...el.files].forEach(f => enqueueFile(f)); el.value = ''; },
    cancelReply:        () => cancelReply(),
    cancelEdit:         () => cancelEdit(),
    cancelFilePreview:  () => cancelFilePreview(),
    confirmFilePreview: () => confirmFilePreview(),

    // ── Voice messages ──
    startVoiceRecording: () => startVoiceRecording(),
    cancelVoiceRecording: () => cancelVoiceRecording(),
    stopAndSendVoice:    () => stopAndSendVoice(),
    toggleVoice:         (e, el) => toggleVoice(el.dataset.a1, el.dataset.a2),
    cycleVoiceRate:      (e, el) => cycleVoiceRate(el),

    // ── Calls ──
    startCall:         (e, el) => startCall(el.dataset.a1 === 'video'),
    acceptCall:        () => acceptCall(),
    rejectCall:        () => rejectCall(),
    endCall:           () => endCall(),
    toggleMute:        () => toggleMute(),
    toggleSpeaker:     () => toggleSpeaker(),
    toggleCamera:      () => toggleCamera(),
    toggleScreenShare: () => toggleScreenShare(),
    minimizeCall:      () => minimizeCall(),
    expandCall:        () => expandCall(),
    // A tap on the video-call image hides or shows the control panel.
    callSurfaceTap:    (e) => callSurfaceTap(e),
    flipCamera:        () => flipCamera(),
    setVolume:         (e, el) => setVolume(el.value),

    // ── Messages: context menu and actions ──
    showCtxMenu:  (e, el) => showCtxMenu(e, el.dataset.a1, el.dataset.a2 === 'true'),
    ctxReply:     () => ctxReply(),
    ctxReact:     () => ctxReact(),
    ctxForward:   () => ctxForward(),
    closeForward: () => closeForward(),
    forwardTyped: () => forwardTyped(),
    ctxEdit:      () => ctxEdit(),
    ctxCopy:      () => ctxCopy(),
    ctxDelete:    () => ctxDelete(),
    ctxDownload:  () => ctxDownload(),
    // ── Chat list: context menu (right click / long press) ──
    // The peer is read from the row's data-peer: rows are re-created on
    // every render, so binding goes through delegation, not a closure.
    showChatMenu: (e, el) => showChatMenu(e, el.dataset.peer),
    cctxPin:      () => cctxPin(),
    cctxMute:     () => cctxMute(),
    cctxArchive:  () => cctxArchive(),
    cctxRead:     () => cctxRead(),
    cctxDelete:   () => cctxDelete(),

    addReaction:    (e, el) => addReaction(el.dataset.a1),
    toggleReaction: (e, el) => toggleReaction(el.dataset.a1, el.dataset.a2),
    scrollToMsg:    (e, el) => scrollToMsg(el.dataset.a1),
    expandMsg:      (e, el) => expandMsg(el.dataset.a1, el.dataset.a2),
    downloadFile:   (e, el) => downloadFile(el.dataset.a1),
    openMedia:      (e, el) => window.open(el.dataset.a1, '_blank', 'noopener'),

    // ── Miscellaneous ──
    openNetworkTest:  () => { closeSettingsMenu(); openNetworkTest(); },
    closeNetworkTest: () => closeNetworkTest(),
    runNetworkTest:   () => runNetworkTest(),
    showE2EPopup:     () => document.getElementById('e2e-popup').classList.add('visible'),
    hideE2EPopup:     () => document.getElementById('e2e-popup').classList.remove('visible'),
    // A click on a modal's backdrop closes it — but only when the backdrop
    // itself was hit, not its content.
    overlayClose: (e, el) => {
      if (e.target !== el) return;
      const act = el.dataset.a1;
      if (act && ACTIONS[act]) ACTIONS[act](e, el);
    },
    noop: () => {},
  };

  function run(e, el) {
    const fn = ACTIONS[el.dataset.act];
    if (typeof fn !== 'function') return;
    if (el.hasAttribute('data-stop')) e.stopPropagation();
    if (el.hasAttribute('data-prevent')) e.preventDefault();
    fn(e, el);
    // Any action inside the call panel extends its visibility, or the panel
    // could leave right after someone pressed "mute".
    if (typeof _armControlsTimer === 'function' && el.closest('.call-box, .flip-cam-btn')) {
      _armControlsTimer();
    }
  }

  // click / contextmenu / mousedown bubble: find the nearest [data-act].
  for (const type of ['click', 'contextmenu', 'mousedown']) {
    document.addEventListener(type, (e) => {
      const attr = type === 'click' ? 'data-act'
                 : type === 'contextmenu' ? 'data-act-ctx' : 'data-act-down';
      const el = e.target.closest(`[${attr}]`);
      if (!el) return;
      if (type === 'click') { run(e, el); return; }
      // Non-click actions carry their name in their own attribute.
      const name = el.getAttribute(attr);
      const fn = ACTIONS[name];
      if (typeof fn !== 'function') return;
      if (el.hasAttribute('data-stop')) e.stopPropagation();
      if (el.hasAttribute('data-prevent')) e.preventDefault();
      fn(e, el);
    });
  }

  // input / change do not bubble from some elements in older browsers, but
  // they do from input/textarea/select everywhere this matters.
  for (const type of ['input', 'change']) {
    document.addEventListener(type, (e) => {
      const attr = type === 'input' ? 'data-act-input' : 'data-act-change';
      const el = e.target.closest(`[${attr}]`);
      if (!el) return;
      const fn = ACTIONS[el.getAttribute(attr)];
      if (typeof fn === 'function') fn(e, el);
    });
  }

  // keydown: the markup declares what Enter should call.
  //   data-act-enter="sendMessage"                 — any Enter
  //   data-act-enter="sendMessage" data-enter-nosh — Enter without Shift
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const el = e.target.closest('[data-act-enter]');
    if (!el) return;
    // Space activates only elements explicitly opted in (a button role on a
    // non-interactive tag).
    if (e.key === ' ' && !el.hasAttribute('data-space')) return;
    if (el.hasAttribute('data-enter-nosh') && e.shiftKey) return;
    const fn = ACTIONS[el.getAttribute('data-act-enter')];
    if (typeof fn !== 'function') return;
    e.preventDefault();
    fn(e, el);
  });

  // Broken avatars are handled without an inline onerror, which CSP forbids.
  // capture: the error event of <img> does not bubble.
  document.addEventListener('error', (e) => {
    const t = e.target;
    if (t && t.tagName === 'IMG' && t.classList.contains('av-img')) t.remove();
  }, true);

  window.HC_ACTIONS = ACTIONS; // for debugging in the console
})();
