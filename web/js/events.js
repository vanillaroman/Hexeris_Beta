// Hexeris — central event delegation.
//
// All interactivity used to hang on inline attributes (onclick="..."), which
// forced Content-Security-Policy to allow script-src 'unsafe-inline' — that is,
// to lose its main point: on any XSS an injected <script> or onerror= would run
// as native code.
// Now the markup carries only a DECLARATION of intent:
//
//   <button data-act="sendMessage">              — no arguments
//   <button data-act="switchTab" data-a1="login">— arguments in data-a1/a2
//   <button data-act="ctxReply" data-stop>       — + stopPropagation
//   <a data-act="downloadFile" data-a1="/files/x" data-prevent>
//
// while binding a name to a function lives here, in ACTIONS. The registry is
// explicit (rather than window[name]) deliberately: the DOM must not be able to
// call an arbitrary global function from a string in an attribute.
//
// The listeners are attached ONCE on document and work for elements created
// later (the message feed, the group panel) — no separate binding after each
// render is needed.

(function () {
  // finePointer — whether the device has a real cursor and a physical keyboard.
  // One check for the whole file: touch and mouse behave differently in more
  // than one place, and every such place must ask the same question.
  const finePointer = () => !!(window.matchMedia &&
    window.matchMedia('(hover: hover) and (pointer: fine)').matches);

  // el — the element carrying data-act, e — the original event.
  const ACTIONS = {
    // ── Auth ──
    doAuth:            () => doAuth(),
    startSSO:          () => startSSO(),
    switchTab:         (e, el) => switchTab(el.dataset.a1),
    togglePasswordVis: (e, el) => {
      const i = document.getElementById('auth-password');
      i.type = i.type === 'password' ? 'text' : 'password';
    },
    submitPasswordChange: () => submitPasswordChange(),
    submitTwoFA:          () => submitTwoFA(),
    cancelTwoFA:          () => cancelTwoFA(),
    open2FASetup:         () => open2FASetup(),
    close2FAModal:        () => close2FAModal(),
    confirm2FAEnable:     () => confirm2FAEnable(),
    start2FADisable:      () => start2FADisable(),
    confirm2FADisable:    () => confirm2FADisable(),
    copyRecoveryCodes:    () => copyRecoveryCodes(),
    reloadPage: () => location.reload(),

    // ── Chats / sidebar ──
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

    // ── Profile / settings ──
    openMyProfile:      () => openMyProfile(),
    closeMyProfile:     () => closeMyProfile(),
    saveMyProfile:      () => saveMyProfile(),
    viewPeerProfile:    () => openPeerProfile(activePeer),
    closePeerProfile:   () => closePeerProfile(),
    pickPresence:       (e, el) => pickPresence(el.dataset.a1),
    pickAvatarFile:     () => document.getElementById('pf-avatar-file').click(),
    uploadAvatar:       (e, el) => uploadAvatar(el),
    toggleSettingsMenu: (e) => toggleSettingsMenu(e),
    // The settings menu items live INSIDE it, so the "click outside" handler
    // does not close them — we close explicitly, the way the former inline code
    // did (closeSettingsMenu();action()).
    toggleNotifications: () => { closeSettingsMenu(); toggleNotifications(); },

    // ── Search ──
    onSearchInput:     () => { if (typeof resetSearchCursor === 'function') resetSearchCursor(); onSearchInput(); },
    toggleChatSearch:  () => toggleChatSearch(),
    // The conversation attachments panel (js/attachpanel.js).
    toggleAttachPanel: () => toggleAttachPanel(),
    attachPanelTab:    (e, el) => attachPanelTab(el.dataset.a1),
    apOpen:            (e, el) => apOpen(el.dataset.a1),
    apMore:            () => apMore(),
    closeChatSearch:   () => closeChatSearch(),
    onChatSearchInput: () => onChatSearchInput(),
    chatSearchStep:    (e, el) => chatSearchStep(Number(el.dataset.a1)),
    // Enter — the next match, Shift+Enter — the previous one (as in the former
    // onChatSearchKey). Escape closes search through the global Escape handler
    // in index.html.
    chatSearchEnter:   (e) => chatSearchStep(e.shiftKey ? -1 : 1),

    // ── Composer ──
    sendMessage:        () => sendMessage(),
    composerInput:      (e, el) => { autoResize(el); sendTyping(); updateComposerState(); },
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
    // A tap on a video-call picture hides/shows the control panel.
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
    // ── Chat list: context menu (right-click / long press) ──
    // peer is read from the row's data-peer: rows are recreated on every render,
    // so the binding goes through delegation rather than a closure.
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
    // The image opens as an overlay on top of the chat rather than in a new
    // tab: in an installed PWA window.open took you to an external browser.
    // The caption in the viewer: the file name, and the sender under it. Only
    // the sender used to be passed, and an opened image did not say what it was
    // called — which is exactly what is needed to find it again later.
    openMedia:      (e, el) => openLightbox(el.dataset.a1,
                       [el.dataset.a2, el.dataset.a3].filter(Boolean).join(' · ')),
    closeLightbox:  () => closeLightbox(),

    // ── Layout and theme ──
    toggleCodeMode: () => toggleCodeMode(),
    cycleTheme:     () => { closeSettingsMenu(); cycleTheme(); },
    scrollFeedToBottom: () => scrollFeedToBottom(),

    // ── Misc ──
    openNetworkTest:  () => { closeSettingsMenu(); openNetworkTest(); },
    cleanupDeletedChats: () => { closeSettingsMenu(); cleanupDeletedChats(); },
    closeNetworkTest: () => closeNetworkTest(),
    runNetworkTest:   () => runNetworkTest(),
    showE2EPopup:     () => document.getElementById('e2e-popup').classList.add('visible'),
    hideE2EPopup:     () => document.getElementById('e2e-popup').classList.remove('visible'),
    // A click on a modal backdrop closes it — only if the backdrop itself was
    // hit, not its content.
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
    // Any action inside the call panel extends how long it is shown — otherwise
    // the panel could slide away right after "mute the microphone" is pressed.
    if (typeof _armControlsTimer === 'function' && el.closest('.call-box, .flip-cam-btn')) {
      _armControlsTimer();
    }
  }

  // click / contextmenu / mousedown — these bubble, so we look for the nearest [data-act].
  for (const type of ['click', 'contextmenu', 'mousedown']) {
    document.addEventListener(type, (e) => {
      const attr = type === 'click' ? 'data-act'
                 : type === 'contextmenu' ? 'data-act-ctx' : 'data-act-down';
      const el = e.target.closest(`[${attr}]`);
      if (!el) return;
      if (type === 'click') { run(e, el); return; }
      // For non-click actions the name lives in its own attribute.
      const name = el.getAttribute(attr);
      const fn = ACTIONS[name];
      if (typeof fn !== 'function') return;
      if (el.hasAttribute('data-stop')) e.stopPropagation();
      if (el.hasAttribute('data-prevent')) e.preventDefault();
      fn(e, el);
    });
  }

  // input / change — these do not bubble on some elements in older browsers,
  // but on input/textarea/select they bubble everywhere we need.
  for (const type of ['input', 'change']) {
    document.addEventListener(type, (e) => {
      const attr = type === 'input' ? 'data-act-input' : 'data-act-change';
      const el = e.target.closest(`[${attr}]`);
      if (!el) return;
      const fn = ACTIONS[el.getAttribute(attr)];
      if (typeof fn === 'function') fn(e, el);
    });
  }

  // Ctrl/Cmd+B and Ctrl/Cmd+I wrap the selection in the composer. Handled here
  // rather than through a data attribute: the shortcut belongs to the input
  // field, not to a markup element, and declaring it in HTML would be untrue.
  document.addEventListener('keydown', (e) => {
    if (!(e.ctrlKey || e.metaKey) || e.altKey) return;
    const k = e.key.toLowerCase();
    if (k !== 'b' && k !== 'i') return;
    if (!e.target || e.target.id !== 'msg-textarea') return;
    e.preventDefault();
    if (typeof wrapSelection === 'function') wrapSelection(k === 'b' ? '**' : '*');
  });

  // keydown: the markup declares WHAT to call on Enter.
  //   data-act-enter="doAuth"            — Enter
  //   data-act-enter="sendMessage" data-enter-nosh — Enter without Shift
  //   data-enter-fine                    — only with a physical keyboard
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    const el = e.target.closest('[data-act-enter]');
    if (!el) return;
    // Space activates only the elements we explicitly allowed it for (a button
    // role on a non-interactive tag).
    if (e.key === ' ' && !el.hasAttribute('data-space')) return;
    if (el.hasAttribute('data-enter-nosh') && e.shiftKey) return;
    // On a phone Enter has no Shift+Enter counterpart: if it sends, there is no
    // way to type a line break at all. There Enter stays Enter — we simply do
    // not stop the browser from inserting a newline.
    if (el.hasAttribute('data-enter-fine') && !finePointer()) return;
    const fn = ACTIONS[el.getAttribute('data-act-enter')];
    if (typeof fn !== 'function') return;
    e.preventDefault();
    fn(e, el);
  });

  // ── Ctrl/Cmd+K — global search ─────────────────────────────────────────
  // The ⌘K badge next to the field had been there for a long time but was only a
  // label. Caught on capture: on some layouts the shortcut otherwise reaches the
  // browser first.
  document.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && (e.key === 'k' || e.key === 'K')) {
      // Only once the user is inside the app: there is no field on the sign-in screen.
      const screen = document.getElementById('chat-screen');
      if (!screen || screen.offsetParent === null) return;
      e.preventDefault();
      if (typeof focusSearch === 'function') focusSearch();
    }
  }, true);

  // Arrows and Enter inside the search field. A separate listener rather than
  // data-act-enter: there Enter is the only action, while here we need the whole
  // set of keys and the order between them.
  document.addEventListener('keydown', (e) => {
    const inp = e.target.closest && e.target.closest('#search-input');
    if (!inp) return;
    if (typeof searchKeydown === 'function') searchKeydown(e);
  });

  // Broken avatars are removed without an inline onerror (that is banned by CSP
  // too). capture: the error event on <img> does not bubble.
  document.addEventListener('error', (e) => {
    const t = e.target;
    if (t && t.tagName === 'IMG' && t.classList.contains('av-img')) t.remove();
  }, true);

  window.HC_ACTIONS = ACTIONS; // for debugging from the console
})();
