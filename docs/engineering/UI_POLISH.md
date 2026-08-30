# Dialogs and readability: what was uneven, and why

An analysis of the task "the dialogs float about; make it even and crisp
everywhere". Everything below was found by measurement on a live page rather
than by reading the code: layout that is "slightly off" cannot be decomposed
into causes by eye.

The instruments are `tests/ui/uitest_dialogs.js` and
`tests/ui/uitest_contrast.js`; both stay in the suite and will not let any of
this come back.

---

## 1. The 2FA dialog moved 154px vertically

The dialog is vertically centred, and the height of each step differs:

| step | height | top edge |
|---|---|---|
| loading | 483 | 159 |
| setup (QR) | 483 | 159 |
| setup + error | 543 | 129 |
| recovery codes | 356 | 222 |
| disabling | 235 | 283 |

The heading moved on every transition — that is what "floating about" meant.
On a phone the same thing, with a spread of 145px.

**What was done.** Dialogs whose content changes while they are open are
anchored to the top instead of being centred (`.dlg-anchored`): 2FA and the
group panel. The dialog becomes a fixed frame with only its bottom growing. The
spread after the change is **0px**, while the step heights are still different
— that is a separate control check: stillness must come from anchoring, not
from there being nothing left to measure.

---

## 2. Dialogs were unreadable in the light theme

`.hex-modal` set its background as a **literal**, `rgba(22,23,29,.72)`,
bypassing the `--glass-*` tokens. In the light theme that produced a dark panel
with dark text: a measured contrast of **1.06** against the WCAG AA requirement
of 4.5.

It affected six dialogs at once: confirm, prompt, own profile, peer profile,
forwarding, network test.

The comment in `web/js/theme.js` warns about exactly this:

> A theme changes ONLY the values of tokens… The moment a literal colour
> appears somewhere, it stays dark in the light theme — and that is discovered
> at the user's end.

The rule is right, but it is not observed by eye: a literal looks fine in the
theme it was picked for. Hence a test that measures.

---

## 3. Muted text fell short of the standard almost everywhere

A sweep over every visible text in both themes found four more places:

| what | was | required | cause |
|---|---|---|---|
| `.chat-empty-sub` | **1.76** | 4.5 | the literal `#3a3b4a` |
| `--muted` on `--bg4` | 4.02 | 4.5 | the token was picked by eye |
| `--muted` on `--bubble-in` | 4.44 | 4.5 | the same |
| `--muted` on glass (light) | 3.04 | 4.5 | glass at `.62` over a darkening layer gave grey 200 rather than white |
| "Sign out" (light) | 3.59 | 4.5 | the light theme did not redefine `--danger-*` and inherited the dark values |

**What was done.** `--muted` was shifted in both themes (lighter in dark, darker
in light), the glass density in the light theme was raised from `.62` to `.92`,
the light theme received its own `--danger-*` and `--warning`, and the literal
in `.chat-empty-sub` was replaced with a token.

After the changes: **zero violations** across six screens in both themes.

---

## 4. There were four dialog widths

340px (`.profile-modal`), 340px (`.nettest-modal`), 360px (`.hex-modal`) and
420px (`.group-box`) — inside one application. Plus two incompatible looks: a
glass one that animates in, and a flat one with no animation, pressed against
the screen edges on a phone.

**What was done.** One surface for every dialog: a shared `--dlg-w` width,
padding, corner radius, shadow and entrance. `.group-box` no longer describes
its own appearance — only a column with an even rhythm between blocks.

The dialog also now **fits on the screen**: its height is bounded by the overlay
frame and the content scrolls inside. Before this, `.group-box` carried
`max-height: 80vh` with no allowance for padding, and on a phone in landscape
the "Turn on" button was out of reach.

---

## 5. The 2FA steps were assembled from inline styles

The markup for each step was written as a string with `font-size`, `color` and
`margin` straight in the attributes. Each step ended up with its own rhythm of
spacing (8, 12 and 14 mixed with `flex-gap`), and its colours were literals.

**What was done.** A set of classes: `.dlg-text`, `.dlg-ok`, `.dlg-warn`,
`.dlg-note`, `.dlg-code`, `.dlg-codes`, `.dlg-qr`, `.dlg-actions`. Not one of
them sets vertical spacing of its own — the rhythm comes from the container's
`gap`, which is why the steps look identically aligned.

Separately: the QR code now has a fixed size **before** the image loads —
otherwise the dialog jerked at the moment it appeared. The secret for manual
entry sits in a frame in a monospace font: people copy it by hand, and an
unbroken 32-character string with nothing to anchor on is read with mistakes.

---

## Tests

`tests/ui/uitest_dialogs.js` — 17 checks. The 2FA scenario is walked through
**for real**: the code is computed from the server-issued secret per RFC 6238.
Faking the "recovery codes" step would have shown an empty grid and slipped
past the very thing the suite was written for.

- the dialog does not move between steps (negative control: restore centring
  and the test fails with a spread of 157px);
- the step heights are different, so there was something to measure;
- dialog contrast in both themes;
- one width for every dialog;
- the dialog fits a phone screen, landscape included.

`tests/ui/uitest_contrast.js` — a sweep over the text on six screens in two
themes. The background is computed as the composition of the **whole** stack of
translucent layers: the first version of the sweeper took the first layer it
found and "saw" a white background on the dark theme, producing false findings.
There is a control check that the sweep reached any text at all.

---

## Second pass: hidden places, the light theme, and a beige palette

### The context menu stayed dark in the light theme

`.ctx-menu` set its background as the literal `rgba(22,23,29,.78)` and its edge
as `rgba(255,255,255,.07)`. Its text comes from `--text` and turned dark with
the theme — a right click or a long press on a contact opened a dark box in
which nothing could be made out. Exactly the same fault as `.hex-modal`, but in
a place that does not catch the eye during a review: the menu is invisible until
you summon it.

### Email and phone vanished from a peer's profile

`.pf-view-row a { color: #fff }` — white links on a light card. The `Email` and
`Phone` rows stayed, the values disappeared. An `--accent-link` token was added
(light in the dark theme, deep blue in the light one): `--accent` could not take
that role — on a dark background it is too dark for text, and in the light theme
the opposite.

### The sweep was extended to everything nobody looks at

To the six screens were added: the chat-list context menu, the message menu, a
peer profile, forwarding, the group dialog, the network test and an expanded
archive — twelve states in each theme. It was that extension that found both of
the faults above.

It also turned up, and fixed: `.nt-fail .nt-detail` (`#e5875b`, 2.37 on light),
`.call-log.missed` and cancelling a voice recording — the same red literal
bypassing `--danger`.

### Escape closed only some of the dialogs

The handler dismissed `#ctx-menu` — the **message** menu — while the chat-list
menu (`#chat-ctx-menu`) stayed up and survived the opening of the next dialog:
the screen ended up holding both the menu and a card on top of it. A peer
profile, forwarding and 2FA did not close at all.

**On 2FA specifically.** Escape could not simply be enabled there "like
everywhere else": `close2FAModal` wipes `_2faCodes`, the codes are shown exactly
once, and after closing the person is left with the second factor enabled and
not a single spare code — losing the phone then means only an administrator can
restore access. On the codes step Escape now does nothing (there is an explicit
"Done" button beside it), and on the others it closes.

The check looks at what is **on the screen** rather than at a variable:
`_2faCodes` lives until the dialog closes and stays populated after moving to
the next step — because of which the first version of the safeguard locked
Escape for the whole dialog to the very end. Caught by a test.

### Two more places found by the extended sweep

`.emoji-picker` — the same literal dark background as the context menu: the
panel stayed dark in the light theme.

The search highlight (`.search-hit .contact-preview mark`) was filled with
`rgba(0,49,83,.8)` and `color: inherit` — in the light theme that produced muted
warm text on dark blue, a contrast of **1.11**. Fill and colour were separated
into the `--mark-bg` / `--mark-fg` tokens.

The sweep also runs on a phone (390×844, touch context): it has its own layout
and `@media (hover: none)` rules, and checking only the desktop would mean
checking half the cases. The mobile pass immediately produced a false finding on
the encryption badge — on a phone it is collapsed into a circle via
`font-size: 0`, so there is no caption on screen at all. The sweeper learned to
skip such text; that is a fix to the instrument, not to a style.

### The light theme: from cold white to warm beige

The project's blue accent reads more calmly on a warm ground: white with
Prussian blue gives an "office" contrast, beige gives a paper one.

| token | was | became |
|---|---|---|
| `--bg` | `#f4f5f8` | `#f2ede3` — warm paper |
| `--bg2` | `#ffffff` | `#fbf8f2` — cream |
| `--bg3` | `#eceef4` | `#eae3d6` |
| `--bg4` | `#e1e4ed` | `#ddd5c5` |
| `--text` | `#1b1d26` | `#2a2620` — a warm near-black |
| `--muted` | `#5a5c72` | `#625b4e` |

One's own bubble stayed blue — it is the identifying mark, and on beige it works
even better. Shadows were recomputed to warm (`rgba(60,50,35,…)`): a cold grey
shadow on beige looks like a dirty smudge. The `theme-color` for the PWA system
bar was brought in line with the new background.

The palette was chosen against measurement rather than by eye: `--muted` was
corrected twice (4.12 against `--bg4`), and `--warning` went from `#9a6a12` to
`#8a5e0f` (4.23 on light). The result is zero violations across twelve screens
in both themes.

---

## What stayed out of scope

- **Literal colours outside the swept screens.** The sweep covers twelve states;
  calls, the lightbox and the file preview are not in it, and literals remain
  there (`.call-btn`, `.e2e-layer-title`, `.msg-status.read`). The call screen
  is deliberately dark in both themes, so some of those are legitimate — but
  separating the legitimate from the forgotten needs measurement, not an
  opinion.
- **Scrolling smoothness and responsiveness** — a separate task with a different
  instrument: contrast and geometry are measured statically, whereas jank has to
  be caught by tracing frames.

---

## Third pass: accessibility, a skeleton, frames, attachments

### The application was silent for blind users

`aria-live` in the client: **zero occurrences**. An incoming message and a
refusal to send were announced in no way at all; the only way to notice a new
message was to run into it with the cursor.

What was done (`web/js/a11y.js`) — and three of these decisions are easy to get
wrong:

- **Two regions, not one.** `polite` waits for a pause in speech, `assertive`
  interrupts. An incoming message must wait: interrupting someone mid-word is
  worse than speaking a second later. "Message not sent" need not wait.
- **Announcements are coalesced.** Twenty messages in a row read out one by one
  is not accessibility, it is torture. Everything inside a 700 ms window becomes
  "3 new messages".
- **The region is hidden with `.sr-only`, not `display:none`.** The latter hides
  the element from the screen reader too, and the live region simply goes
  silent.

Focus: dialogs take it on opening and return it on closing, and `Tab` does not
wander onto elements underneath. The observation watches the dialogs themselves
(`MutationObserver`) rather than every point at which one is opened: a new
dialog gets the behaviour automatically, and it cannot be forgotten.

### The list claimed there were no conversations

This turned out to be worse than "an empty screen". While the history was
loading, the list said **"No conversations yet"** — not an absence of
information but an incorrect statement. A person reads that as "everything is
gone".

Now, until loading finishes, there is a skeleton with **the same geometry** as
the future rows: a 44 px avatar and two lines of text. That is what a skeleton
is for — so that the list does not jump when the data is substituted — rather
than to make waiting pretty.

Something else surfaced: on a first sign-in (when there is no local cache yet)
the list was not drawn **at all** until the server answered. `renderContacts` is
now also called at the start of loading.

### Smoothness: the instrument came before the gallery

`tests/ui/uitest_frames.js` collects inter-frame intervals while scrolling a
conversation of 400 messages. The order is deliberate: a baseline is taken
**before** hundreds of thumbnails are added to the screen, not after.

| | median | p95 | long frames |
|---|---|---|---|
| desktop | 16.7 ms | 16.7 ms | 0 of 89 |
| phone | 16.7 ms | 16.8 ms | 1 of 89 |

So scrolling runs at 60 frames per second, and the word "laggy" did not apply to
it. The thresholds in the suite are lenient (p95 ≤ 100 ms): the task is to catch
a regression when the list starts redrawing wholesale on every frame, not to
certify a frame rate. A threshold that fails every other run stops being read.

### The attachments panel

Media / Files / Voice tabs in the conversation header, pages of 60, viewed with
the existing lightbox — there is no reason to introduce a second viewer with its
own key handling and its own way of closing.

The server filters by `media_type` (an open column; two partial indexes were
added) and decrypts **only the page it returns**. The access boundary is
re-checked rather than inherited from history: the endpoint hands out links to
files, so a test for "another person's conversation is not served" is not a
formality here.

The "Filter by name" field sifts what has been loaded, and is labelled as such.
Promising a full search and quietly searching one page is worse than not
promising: the person will conclude the file is not there.

**Two faults found by tests and invisible to the eye:**

1. The panel slid underneath the floating header — the tabs could not be
   pressed, and the peer's name intercepted the click. The header is
   translucent, so visually everything looked fine. It is compensated by the
   same `--feed-pad-*` as the feed.
2. The file name lives in the `#fragment` of the message body (on disk the file
   sits under a random hash — knowing the moment of upload must not make the URL
   guessable). The panel was reading the path and would have shown the hash,
   with the filter searching that same hash.

The panel was added to the readability sweep **immediately**: "we will add it to
the checks later" is exactly what let in the literal colours that broke the
light theme.
