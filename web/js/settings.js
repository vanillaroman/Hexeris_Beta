// Hexeris — the sidebar settings menu (the overflow menu beside the profile).
// It collects the secondary actions that used to take up space in the footer:
// network test, the notifications toggle and sign-out. Opens from the "⋯"
// button and closes on an outside click or Escape.
//
// .dropdown-menu / .dropdown-item are a reusable primitive (see app.css).

function toggleSettingsMenu(e) {
  if (e) e.stopPropagation();
  const menu = document.getElementById('settings-menu');
  if (!menu) return;
  if (menu.classList.contains('open')) { closeSettingsMenu(); return; }
  // Refresh the notifications toggle before showing the menu.
  if (typeof refreshNotifyState === 'function') refreshNotifyState();
  menu.classList.add('open');
  const btn = document.getElementById('settings-btn');
  if (btn) btn.setAttribute('aria-expanded', 'true');
  // Closing on an outside click or Escape is bound on the next tick, so the
  // click that opened the menu does not close it immediately.
  setTimeout(() => {
    document.addEventListener('click', _settingsOutside);
    document.addEventListener('keydown', _settingsEsc);
  }, 0);
}

function closeSettingsMenu() {
  const menu = document.getElementById('settings-menu');
  if (menu) menu.classList.remove('open');
  const btn = document.getElementById('settings-btn');
  if (btn) btn.setAttribute('aria-expanded', 'false');
  document.removeEventListener('click', _settingsOutside);
  document.removeEventListener('keydown', _settingsEsc);
}

function _settingsOutside(e) {
  const wrap = document.querySelector('.sidebar-settings');
  if (wrap && !wrap.contains(e.target)) closeSettingsMenu();
}

function _settingsEsc(e) {
  if (e.key === 'Escape') closeSettingsMenu();
}
