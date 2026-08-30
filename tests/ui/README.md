# UI suites

Twenty-seven suites of checks against a live instance. Their purpose is not
"coverage for coverage's sake" but pinning down the concrete scenarios that have
already broken at least once.

## Running them

```bash
HEXERIS_URL=http://localhost:8080 ./run-all.sh
```

The instance must be started with `REGISTRATION_ENABLED=true` and a raised
`REGISTER_MAX_PER_IP` — the suites create test users of their own.

## What is checked

### Baseline behaviour

| Suite | Scenario |
|---|---|
| `uitest.js` | The baseline path: sign-in, sending, delivery |
| `uitest_ui.js` | Markup in messages, themes, code blocks |
| `uitest_authscreen.js` | The sign-in screen: no flash, no field jumps |
| `uitest_sso.js` | Sign-in through an external provider |
| `uitest_twofa.js` | The whole two-factor sign-in, with real TOTP codes |
| `uitest_contactlist.js` | The conversation list: order, counters, pinning |
| `uitest_realtime.js` | Live delivery: two clients, statuses, reactions |
| `uitest_sw.js` | The service worker: shell, version, update |

### Phone and gestures

| Suite | Scenario |
|---|---|
| `uitest_touch.js` | Long press on a phone, context menus |
| `uitest_mobile_composer.js` | The whole input panel on a phone |
| `uitest_safearea.js` | Padding for the system bar, including a cut-out |
| `uitest_back.js` | The "back" button closes layers rather than leaving the site |
| `uitest_scroll.js` | Scrollbar visibility, the edge of a code block |

### Appearance and response

| Suite | Scenario |
|---|---|
| `uitest_polish.js` | Interface response, hover states |
| `uitest_dialogs.js` | One surface for every window, the 2FA window does not wander between steps |
| `uitest_contrast.js` | Text contrast against WCAG AA: 16 desktop states and 4 phone ones, both themes |
| `uitest_frames.js` | Frame tracing: stutters when opening layers and while scrolling |
| `uitest_perf.js` | Incremental rendering, grouping, Ctrl+K |

### Data and network

| Suite | Scenario |
|---|---|
| `uitest_coldstart.js` | Cold start: a clean browser, 12 contacts × 950 messages — all in one sign-in |
| `uitest_sync.js` | History sync: saving to storage and surviving a reload |
| `uitest_attachments.js` | The attachments panel: tabs, filter, paging, following the conversation |
| `uitest_linkpreview.js` | Link previews: hopeless addresses are not asked about, a refusal is remembered |
| `uitest_gone.js` | Deleted accounts: no asking in circles — and a returned person visible at once |
| `uitest_hostile.js` | Hostile paths: XSS, races, reloads, losing the network |

### Other

| Suite | Scenario |
|---|---|
| `uitest_fwdbug.js` | Forwarding: cancel → repeat → confirm |
| `uitest_round3.js` | Photo viewing, forward confirmation, avatars |
| `uitest_adminpanel.js` | The administrator panel: sign-in, lists, export |

## The principle

Every check comes with a **control**: proof that it fails on broken code. A check
that always passes is worse than a missing one — it creates false confidence. In
several places a control has already caught an empty check, and that is recorded
in the comments inside the suites.

`uitest_sync.js` carries a lesson of its own: an error inside `loadHistory` landed
in a `catch`, wrote a line to the console and carried on — on screen everything
looked right, while the history never reached storage and disappeared on the next
sign-in. So the suites check not "there are rows on screen" but the state that
breaks silently: the contents of `localStorage`, the number of network requests,
the absence of warnings in the console.
