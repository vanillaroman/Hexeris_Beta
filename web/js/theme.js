// Hexeris — light/dark theme.
//
// The choice is set as the data-theme attribute on <html>, and the whole
// palette is described by tokens in css/app.css. There is one rule: a theme
// changes ONLY token values, so every other rule knows nothing about it. As
// soon as a literal colour appears somewhere it stays dark in the light theme —
// and that gets discovered at the user's end.
//
// Three states: 'dark', 'light' and 'system'. The default is DARK, not system.
// This is not about looks: dark was the only theme all along, and defaulting to
// 'system' would make the app change its appearance by itself for everyone
// whose OS is light — simply because an update shipped. Adding a setting must
// not change how the product looks for people who never touched it. 'system'
// stays in the cycle for those who choose it deliberately.

const THEME_KEY = 'hc_theme';
const THEME_ORDER = ['dark', 'light', 'system'];

function storedTheme() {
  try { return localStorage.getItem(THEME_KEY) || 'dark'; } catch { return 'dark'; }
}

// effectiveTheme — what we actually display.
function effectiveTheme(mode) {
  if (mode === 'dark' || mode === 'light') return mode;
  return (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches)
    ? 'light' : 'dark';
}

function applyTheme(mode) {
  const eff = effectiveTheme(mode);
  const root = document.documentElement;
  // Dark is the base in CSS, so the attribute is set only for light: with JS
  // disabled or before it loads the page stays dark instead of flashing white.
  if (eff === 'light') root.setAttribute('data-theme', 'light');
  else root.removeAttribute('data-theme');

  // theme-color affects the system bar colour in a PWA and the address bar on
  // Android. Without it a dark strip stays at the top in the light theme.
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', eff === 'light' ? '#f2ede3' : '#0f1015');

  updateThemeMenuItem(mode, eff);
}

function setTheme(mode) {
  if (THEME_ORDER.indexOf(mode) < 0) mode = 'system';
  try { localStorage.setItem(THEME_KEY, mode); } catch {}
  applyTheme(mode);
}

// cycleTheme — one menu item instead of three: dark → light → system → dark.
// The label always shows the current state, so the cycle does not turn into
// guesswork.
function cycleTheme() {
  const next = THEME_ORDER[(THEME_ORDER.indexOf(storedTheme()) + 1) % THEME_ORDER.length];
  setTheme(next);
  if (typeof toast === 'function') {
    toast(next === 'system' ? 'Theme: follows your system' : 'Theme: ' + next, 'success');
  }
}

const THEME_LABEL = { system: 'System', dark: 'Dark', light: 'Light' };

function updateThemeMenuItem(mode, eff) {
  const item = document.getElementById('settings-theme');
  if (!item) return;
  const label = item.querySelector('.dd-label');
  if (label) {
    label.textContent = 'Theme: ' + (THEME_LABEL[mode] || 'Dark') +
      (mode === 'system' ? ' (' + eff + ')' : '');
  }
  // The icon changes with the theme — a sun for light, a moon for dark.
  const icon = item.querySelector('svg');
  if (icon) icon.setAttribute('data-theme-icon', eff);
}

function initTheme() {
  applyTheme(storedTheme());
  // While 'system' is selected we follow OS theme changes live: a person
  // switches the OS to dark in the evening and expects the same from the app.
  if (window.matchMedia) {
    const mq = window.matchMedia('(prefers-color-scheme: light)');
    const onChange = () => { if (storedTheme() === 'system') applyTheme('system'); };
    if (mq.addEventListener) mq.addEventListener('change', onChange);
    else if (mq.addListener) mq.addListener(onChange);
  }
}

// The theme is applied while the script is parsed, without waiting for
// DOMContentLoaded: otherwise a foreign background flashes before it applies.
initTheme();
