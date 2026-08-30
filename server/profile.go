package main

// Employee profiles: display name, position, avatar and the manually chosen
// presence status (available / busy / away).
//
// presence is what the user sets themselves; it lives in the database and
// survives reconnects. online/offline is the separate fact of having a live
// WS connection (see broadcastStatus in auth.go). The client merges both
// signals into one indicator.
//
// Endpoints, all requiring a valid token:
//
//	GET  /api/profile              own profile
//	GET  /api/profile?user=NAME    another user's public profile
//	GET  /api/profiles             profiles of every peer, in one request
//	POST /api/profile              update own profile
//	POST /api/presence             change own presence
//
// Updates are pushed to peers as a type="profile" WS message so contact lists
// and chat headers refresh without a reload.

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Profile is a user's public card; Online is computed on the fly.
type Profile struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Position    string `json:"position"`
	AvatarURL   string `json:"avatar_url"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	Presence    string `json:"presence"` // available | busy | away
	Online      bool   `json:"online"`
	// MustChangePassword is filled in only for one's own profile. Session
	// restore needs it: closing the tab without changing an admin-issued
	// password must not clear the requirement, or a page reload bypasses it.
	MustChangePassword bool `json:"must_change_password,omitempty"`
}

const (
	maxDisplayNameLen = 64
	maxPositionLen    = 64
	maxContactLen     = 128 // email / phone
)

var validPresence = map[string]bool{"available": true, "busy": true, "away": true}

func isOnline(username string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return len(clients[username]) > 0
}

// loadProfile reads a profile; a missing row surfaces as sql.ErrNoRows.
func loadProfile(username string) (Profile, error) {
	var p Profile
	err := db.QueryRow(
		`SELECT username, display_name, position, avatar_url, email, phone, presence
		   FROM users WHERE username=$1`, username,
	).Scan(&p.Username, &p.DisplayName, &p.Position, &p.AvatarURL, &p.Email, &p.Phone, &p.Presence)
	if err != nil {
		return p, err
	}
	if p.Presence == "" {
		p.Presence = "available"
	}
	p.Online = isOnline(username)
	return p, nil
}

func profileHandler(w http.ResponseWriter, r *http.Request) {
	me, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		target := r.URL.Query().Get("user")
		if target == "" {
			target = me
		}
		if !usernameRe.MatchString(target) {
			http.Error(w, "bad username", http.StatusBadRequest)
			return
		}
		// The endpoint answers for an arbitrary name, so without a limit it
		// is a convenient scanner of the user base.
		if target != me && profileLookupLimiter.isBlocked(me) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		p, err := loadProfile(target)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if target == me {
			p.MustChangePassword = mustChangePassword(me)
		}
		if target != me {
			profileLookupLimiter.recordFailure(me)
			// Contact details go only to people with shared context (a
			// conversation or a common group). Serving email and phone for
			// any user to any signed-in caller makes the whole staff
			// directory, personal data included, downloadable. Name,
			// position and avatar stay visible — they are what one needs to
			// find a colleague and start a conversation.
			if !hasContactWith(me, target) {
				p.Email, p.Phone = "", ""
			}
		}
		writeJSON(w, p)

	case http.MethodPost:
		var req struct {
			DisplayName string `json:"display_name"`
			Position    string `json:"position"`
			AvatarURL   string `json:"avatar_url"`
			Email       string `json:"email"`
			Phone       string `json:"phone"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.DisplayName = strings.TrimSpace(req.DisplayName)
		req.Position = strings.TrimSpace(req.Position)
		req.Email = strings.TrimSpace(req.Email)
		req.Phone = strings.TrimSpace(req.Phone)
		if utf8.RuneCountInString(req.DisplayName) > maxDisplayNameLen ||
			utf8.RuneCountInString(req.Position) > maxPositionLen {
			http.Error(w, "display name / position too long", http.StatusBadRequest)
			return
		}
		if utf8.RuneCountInString(req.Email) > maxContactLen ||
			utf8.RuneCountInString(req.Phone) > maxContactLen {
			http.Error(w, "email / phone too long", http.StatusBadRequest)
			return
		}
		// An avatar may only be a file uploaded here. External URLs and
		// javascript:/data: values are rejected: otherwise a planted avatar
		// becomes an XSS and tracking vector.
		if req.AvatarURL != "" && !strings.HasPrefix(req.AvatarURL, "/files/") {
			http.Error(w, "avatar must be an uploaded file", http.StatusBadRequest)
			return
		}
		_, err := db.Exec(
			`UPDATE users SET display_name=$1, position=$2, avatar_url=$3, email=$4, phone=$5 WHERE username=$6`,
			req.DisplayName, req.Position, req.AvatarURL, req.Email, req.Phone, me)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		p, _ := loadProfile(me)
		safeGo("broadcastProfile", func() { broadcastProfile(p) })
		writeJSON(w, p)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// profilesHandler returns every peer's profile at once, so the client can
// fill its contact list with names, avatars and statuses in one request.
func profilesHandler(w http.ResponseWriter, r *http.Request) {
	me, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	peers := peerList(me)
	out := make([]Profile, 0, len(peers))
	for _, peer := range peers {
		if p, err := loadProfile(peer); err == nil {
			out = append(out, p)
		}
	}
	writeJSON(w, out)
}

func presenceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	me, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Presence string `json:"presence"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || !validPresence[req.Presence] {
		http.Error(w, "presence must be available|busy|away", http.StatusBadRequest)
		return
	}
	if _, err := db.Exec(`UPDATE users SET presence=$1 WHERE username=$2`, req.Presence, me); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	p, _ := loadProfile(me)
	safeGo("broadcastProfile", func() { broadcastProfile(p) })
	writeJSON(w, map[string]string{"presence": req.Presence})
}

// broadcastProfile pushes an updated profile to every peer so their UI
// refreshes without a reload.
func broadcastProfile(p Profile) {
	msg := Message{
		Type: "profile", From: p.Username,
		DisplayName: p.DisplayName, Position: p.Position,
		AvatarURL: p.AvatarURL, Presence: p.Presence,
	}
	data, _ := json.Marshal(msg)
	peers := peerList(p.Username)
	mu.RLock()
	defer mu.RUnlock()
	for _, peer := range peers {
		for _, c := range clients[peer] {
			c.send(data)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
