package main

// Web Push for the PWA on iOS, Android and desktop.
//
// Push payloads deliberately carry only non-secret metadata (the sender's
// username) and never message content: notifications pass through a vendor
// push service outside this deployment's control.

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Generate a VAPID keypair once and keep it: existing subscriptions stay
// valid only while the key is unchanged, and rotating it forces every client
// to subscribe again.
var (
	vapidPublic  = os.Getenv("VAPID_PUBLIC_KEY")
	vapidPrivate = os.Getenv("VAPID_PRIVATE_KEY")
	vapidSubject = func() string {
		if s := os.Getenv("VAPID_SUBJECT"); s != "" {
			return s
		}
		if s := os.Getenv("VAPID_CONTACT"); s != "" {
			return s
		}
		return "mailto:admin@localhost"
	}()
)

func pushEnabled() bool { return vapidPublic != "" && vapidPrivate != "" }

func initPush() {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS push_subscriptions (
			id         SERIAL PRIMARY KEY,
			username   TEXT NOT NULL,
			endpoint   TEXT NOT NULL UNIQUE,
			p256dh     TEXT NOT NULL,
			auth       TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`)
	if err != nil {
		log.Println("create push_subscriptions:", err)
		return
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_push_user ON push_subscriptions(username)`)
	if pushEnabled() {
		log.Println("Web Push enabled")
	} else {
		log.Println("Web Push DISABLED (VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY not set)")
	}
}

// Public by design: the client needs the key before it can subscribe.
func vapidPublicKeyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"publicKey": vapidPublic})
}

// The username comes from the JWT and is never trusted from the body.
func subscribeHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// DELETE unsubscribes a device, scoped to the caller's own rows, so
	// knowing someone else's endpoint URL does not unsubscribe them.
	if r.Method == http.MethodDelete {
		var req struct {
			Endpoint string `json:"endpoint"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.Endpoint == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		db.Exec(`DELETE FROM push_subscriptions WHERE endpoint=$1 AND username=$2`,
			req.Endpoint, username)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var sub struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		http.Error(w, "invalid subscription", http.StatusBadRequest)
		return
	}
	// The endpoint must be an external https URL: the server POSTs to it,
	// so an unvalidated value turns push into blind SSRF against arbitrary
	// addresses, internal ones included.
	if u, err := url.Parse(sub.Endpoint); err != nil || u.Scheme != "https" || u.Host == "" {
		http.Error(w, "invalid endpoint", http.StatusBadRequest)
		return
	}
	// Upsert by endpoint; re-bind to the current user if it moved.
	_, err := db.Exec(`
		INSERT INTO push_subscriptions(username, endpoint, p256dh, auth)
		VALUES($1,$2,$3,$4)
		ON CONFLICT(endpoint) DO UPDATE SET username=$1, p256dh=$3, auth=$4`,
		username, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	// Cap devices per account: subscriptions otherwise accumulate forever,
	// and each one costs an extra POST on every notification.
	db.Exec(`DELETE FROM push_subscriptions WHERE username=$1 AND id NOT IN (
	         SELECT id FROM push_subscriptions WHERE username=$1
	         ORDER BY created_at DESC LIMIT 10)`, username)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
	// Kind marks call pushes so the service worker can render Accept/Decline
	// actions; From lets the notification deep-link straight to that caller.
	Kind string `json:"kind,omitempty"`
	From string `json:"from,omitempty"`
}

// notifyOfflinePush sends a push to every device registered for `to`.
// Call it (ideally in a goroutine) when `to` has no live WebSocket connection.
// `from` is the sender's username; `mediaType` is non-empty for attachments.
func notifyOfflinePush(to, from, mediaType string) {
	if !pushEnabled() {
		return
	}
	body := "New message from " + from
	if mediaType != "" {
		body = "📎 Attachment from " + from
	}
	payload, _ := json.Marshal(pushPayload{
		Title: cfg.AppName,
		Body:  body,
		URL:   "/",
		Tag:   "msg-" + from,
	})

	sendPushTo(to, payload, 86400)
}

// notifyCallPush sends the incoming-call notification with a short TTL: a
// call push that arrives five minutes late must not wake a phone, since by
// then nobody is still ringing.
func notifyCallPush(to, from string) {
	if !pushEnabled() {
		return
	}
	payload, _ := json.Marshal(pushPayload{
		Title: cfg.AppName,
		Body:  "Incoming call from " + from,
		// Deep-link straight to the caller so tapping the notification (or its
		// Accept action) opens the app already pointed at this call.
		URL:  "/?call=" + url.QueryEscape(from),
		Tag:  "call-" + from,
		Kind: "call",
		From: from,
	})
	sendPushTo(to, payload, int(callQueueTTL.Seconds()))
}

// The same SSRF-safe client unfurl uses: even if a row somehow holds an
// endpoint pointing at an internal address, resolution into a private range
// is refused at dial time.
var pushHTTPClient = sync.OnceValue(newSafeHTTPClient)

func sendPushTo(to string, payload []byte, ttl int) {
	rows, err := db.Query(`SELECT endpoint, p256dh, auth FROM push_subscriptions WHERE username=$1`, to)
	if err != nil {
		return
	}
	type sub struct{ endpoint, p256dh, auth string }
	var subs []sub
	for rows.Next() {
		var s sub
		if rows.Scan(&s.endpoint, &s.p256dh, &s.auth) == nil {
			subs = append(subs, s)
		}
	}
	rows.Close()

	for _, s := range subs {
		resp, err := webpush.SendNotification(payload, &webpush.Subscription{
			Endpoint: s.endpoint,
			Keys:     webpush.Keys{P256dh: s.p256dh, Auth: s.auth},
		}, &webpush.Options{
			HTTPClient:      pushHTTPClient(),
			Subscriber:      vapidSubject,
			VAPIDPublicKey:  vapidPublic,
			VAPIDPrivateKey: vapidPrivate,
			TTL:             ttl,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			log.Println("push send error:", err)
			continue
		}
		// 404/410 => the subscription is gone; drop it so we stop trying.
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			db.Exec(`DELETE FROM push_subscriptions WHERE endpoint=$1`, s.endpoint)
		}
		resp.Body.Close()
	}
}
