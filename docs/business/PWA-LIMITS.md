# The mobile version: what it can and cannot do

On a phone, Hexeris is a PWA: the application installs from the browser and
runs in its own window without an address bar. There are **no** native
applications in the App Store or on Google Play.

This document deliberately starts with the limitations. A customer will meet
them in the second week anyway, and it is better that they hear about them
from us.

---

## Limitations

| Limitation | Android | iPhone | Comment |
|---|---|---|---|
| Installation only from the browser | ✅ works | ⚠️ via "Share → Add to Home Screen" | On iPhone the menu item is not obvious; briefing the users is essential |
| Push while the app is closed | ✅ works | ⚠️ only once installed to the home screen | In an ordinary Safari tab, push does not arrive at all |
| Push while the screen is off | ✅ | ⚠️ sometimes delayed | iOS suspends web applications aggressively |
| Background sync | ⚠️ limited | ❌ | After a long idle period, history is fetched when the app is opened |
| Calls while the app is in the background | ❌ | ❌ | An incoming call appears as a push; it can be answered by opening the app |
| Unread badge on the icon | ✅ | ❌ | iOS does not expose the Badging API to web applications |
| Access to phone contacts | ❌ | ❌ | Neither requested nor needed |
| Fully offline operation | ⚠️ partial | ⚠️ partial | It opens and shows what has been loaded; anything sent goes out when the network returns |

---

## What works reliably

- **Installation to the home screen** — an icon, its own splash screen, a
  separate window.
- **Sending without a network.** Anything written while the connection is down
  goes into a persistent queue and leaves by itself once it returns. The
  message survives closing the tab — the queue outlives a restart.
- **Push notifications for messages and calls** — on Android with the app
  closed; on iPhone once it is installed to the home screen.
- **Opening without a network**: the interface and already-loaded conversations
  are available.
- **Returning to the app**: the system back button closes whatever layer is
  open (a photo, a dialog, a chat) rather than throwing the user out of the
  application.

---

## Why a PWA rather than native applications

A considered choice, not a saving:

1. **Updates are immediate and identical for everyone.** A fix reaches
   employees the next time they open the app, with no store review and no "half
   of them are still on the old version".
2. **No dependency on Apple or Google.** For a customer on a closed network
   this is decisive: the application cannot be pulled from a store, and
   installing it does not require an account with someone else's service.
3. **One codebase for every platform** — for a team this size, two native
   codebases would mean both falling behind.

The price is the table above. The ones that matter most: the unread badge on
iPhone, and answering a call while the app is in the background.

---

## If a native application is essential

That is negotiable, but let us be straight about it: building and maintaining
two native clients is a project comparable in size to the messenger itself. If
push with the screen off on iPhone, or answering a call from the background, is
a blocker for your scenario, say so **before** the pilot rather than at the end
of it.

---

## A check to run before the pilot

Give five employees with different phones — at least one iPhone — a day to
install the app and message each other. That removes 90% of the questions that
would otherwise come later, and it costs one day rather than three weeks.
