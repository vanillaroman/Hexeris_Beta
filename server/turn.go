package main

// Ephemeral TURN credentials (coturn REST API).

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Credentials follow coturn's use-auth-secret scheme: username =
// "<unix-expiry>:<user>", credential = base64(HMAC-SHA1(secret, username)).
// Static credentials would be public in client JS and let anyone relay
// traffic through the TURN server. Without TURN_SECRET this returns 503 and
// the client falls back to STUN only.

var turnSecret = os.Getenv("TURN_SECRET")

func turnCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	user, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if turnSecret == "" {
		http.Error(w, "turn not configured", http.StatusServiceUnavailable)
		return
	}
	const ttl = 4 * time.Hour // comfortably longer than any single call
	turnUser := fmt.Sprintf("%d:%s", time.Now().Add(ttl).Unix(), user)
	mac := hmac.New(sha1.New, []byte(turnSecret))
	mac.Write([]byte(turnUser))
	cred := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"urls":       cfg.TurnURLs,
		"username":   turnUser,
		"credential": cred,
		"ttl":        int(ttl.Seconds()),
	})
}
