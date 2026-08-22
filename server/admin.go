package main

// Admin API: user management and an audit trail.
//
// The panel is static and usually hosted on a separate origin, so these
// endpoints emit CORS headers. The key travels in the X-Admin-Key header; in
// a query string it would surface in access logs and browser history.

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"

	"fmt"
	"github.com/lib/pq"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// A second barrier after the key: a leaked ADMIN_KEY is useless from an
// unlisted address. While ADMIN_ALLOWED_IPS is empty the filter blocks
// nothing but logs the addresses it sees, so an operator can learn what to
// list without locking themselves out.
//
// Note that when the panel is static and the browser calls the API directly,
// the address seen here is the administrator's own, not the panel's host.
var adminAllowedIPs = func() map[string]bool {
	out := map[string]bool{}
	for _, ip := range strings.Split(os.Getenv("ADMIN_ALLOWED_IPS"), ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			out[ip] = true
		}
	}
	return out
}()

func adminIPAllowed(r *http.Request) bool {
	ip := getIP(r)
	if len(adminAllowedIPs) == 0 {
		log.Printf("ADMIN access from %s (IP filter disabled; set ADMIN_ALLOWED_IPS to enable)", ip)
		return true
	}
	if adminAllowedIPs[ip] {
		return true
	}
	log.Printf("ADMIN BLOCKED from %s (not in ADMIN_ALLOWED_IPS)", ip)
	return false
}

// keyFingerprint is eight hex characters of SHA-256, enough to compare keys
// in a log without printing them. An absent key is labelled rather than
// hashed: "no header at all" is its own diagnosis and must not look like
// "some key arrived".
func keyFingerprint(s string) string {
	if s == "" {
		return "(empty)"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// adminGuard sets CORS, answers preflight and verifies the key. It returns
// false when the request must not be handled further.
func adminGuard(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", cfg.AdminOrigin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "X-Admin-Key, Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	// The header is the supported form; the query parameter remains only
	// for older panel builds and should be dropped once they are updated.
	key := r.Header.Get("X-Admin-Key")
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(adminKey())) != 1 {
		// The response says nothing specific: whoever lacks the key has no
		// business learning why they were refused. The log, however, must
		// distinguish a key mismatch from an IP rejection — both are 403
		// from the panel's side.
		//
		// Only fingerprints are logged. Logs are read more widely than
		// config, and eight hex characters of SHA-256 reveal nothing about
		// a 256-bit random key while still answering the one question that
		// arises here: is this the same key or a different one. Lengths
		// cannot answer it, since two keys generated the same way are both
		// the same length and one of them is still wrong.
		log.Printf("ADMIN BLOCKED from %s: X-Admin-Key mismatch "+
			"(received len=%d sha=%s, expected len=%d sha=%s)",
			getIP(r), len(key), keyFingerprint(key), len(adminKey()), keyFingerprint(adminKey()))
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	// The key already checked out, so this message can be specific: only
	// someone holding the key sees it, and it saves an operator an hour of
	// telling two identical-looking refusals apart.
	if !adminIPAllowed(r) {
		http.Error(w, "forbidden: source IP "+getIP(r)+" is not in ADMIN_ALLOWED_IPS",
			http.StatusForbidden)
		return false
	}
	return true
}

// initAdminSchema creates what only the admin API needs.
func initAdminSchema() {
	initLoginAuditSchema()
	// Blocking rather than deleting: their messages stay in everyone
	// else's history while they can no longer sign in.
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS blocked BOOLEAN NOT NULL DEFAULT FALSE`)
	// forced_logout_at invalidates issued JWTs: tokens whose iat is not
	// later than this mark stop validating (see validateToken).
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS forced_logout_at BIGINT NOT NULL DEFAULT 0`)

	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS admin_audit (
			id         BIGSERIAL PRIMARY KEY,
			action     TEXT NOT NULL,
			target     TEXT NOT NULL,
			details    TEXT NOT NULL DEFAULT '',
			ip         TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`)
	if err != nil {
		log.Println("create admin_audit:", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_admin_audit_created ON admin_audit(created_at DESC)`)
}

// audit records an administrative action, without which "who blocked this
// user, and when" has no answer.
func audit(r *http.Request, action, target, details string) {
	db.Exec(`INSERT INTO admin_audit(action, target, details, ip) VALUES($1,$2,$3,$4)`,
		action, target, details, getIP(r))
	log.Printf("ADMIN %s target=%s %s from=%s", action, target, details, getIP(r))
}

type adminUser struct {
	Username string `json:"username"`
	// Profile fields, as shown in the panel and exported to CSV.
	DisplayName string `json:"display_name"`
	Position    string `json:"position"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	CreatedAt   int64  `json:"created_at"`
	Blocked     bool   `json:"blocked"`
	Online      bool   `json:"online"`
	Messages    int    `json:"messages"`
	LastSeen    int64  `json:"last_seen"`
}

// One column list for every filter branch: copied per branch, adding a field
// takes three synchronised edits and a mismatch shows up as an empty column
// under one filter only. The order must match scanAdminUser below.
const adminUserCols = `
			u.username, u.display_name, u.position, u.email, u.phone,
			EXTRACT(EPOCH FROM u.created_at)*1000, u.blocked,
			(SELECT COUNT(*) FROM messages m WHERE m.sender=u.username),
			COALESCE((SELECT EXTRACT(EPOCH FROM MAX(m.created_at))*1000 FROM messages m WHERE m.sender=u.username),0)`

func scanAdminUser(rows *sql.Rows) (adminUser, error) {
	var u adminUser
	var created, last float64
	err := rows.Scan(&u.Username, &u.DisplayName, &u.Position, &u.Email, &u.Phone,
		&created, &u.Blocked, &u.Messages, &last)
	u.CreatedAt = int64(created)
	u.LastSeen = int64(last)
	return u, err
}

// GET /admin/users?q=<search>&limit=&offset=
func adminUsersHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := 50, 0
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// Message count and last activity come from subqueries rather than
	// pulling the messages table into memory.
	filter := r.URL.Query().Get("filter") // online | blocked | ""
	var rows *sql.Rows
	var err error
	// The online filter takes its list from the in-memory clients map and
	// passes it into SQL, or LIMIT would cut users before filtering.
	var onlineList []string
	if filter == "online" {
		mu.RLock()
		for u := range clients {
			if len(clients[u]) > 0 {
				onlineList = append(onlineList, u)
			}
		}
		mu.RUnlock()
		if len(onlineList) == 0 {
			// Nobody online: answer immediately.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"users": []adminUser{}, "total": 0})
			return
		}
	}

	switch filter {
	case "blocked":
		rows, err = db.Query(`SELECT`+adminUserCols+`
			FROM users u WHERE u.blocked=TRUE AND ($1='' OR u.username ILIKE '%'||$1||'%')
			ORDER BY u.created_at DESC LIMIT $2 OFFSET $3`, q, limit, offset)
	case "online":
		rows, err = db.Query(`SELECT`+adminUserCols+`
			FROM users u WHERE u.username = ANY($1) AND ($2='' OR u.username ILIKE '%'||$2||'%')
			ORDER BY u.created_at DESC LIMIT $3 OFFSET $4`,
			pq.Array(onlineList), q, limit, offset)
	default:
		rows, err = db.Query(`SELECT`+adminUserCols+`
			FROM users u WHERE ($1='' OR u.username ILIKE '%'||$1||'%')
			ORDER BY u.created_at DESC LIMIT $2 OFFSET $3`, q, limit, offset)
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []adminUser{}
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			continue
		}
		mu.RLock()
		u.Online = len(clients[u.Username]) > 0
		mu.RUnlock()

		out = append(out, u)
	}

	var total int
	switch filter {
	case "online":
		total = len(onlineList)
	case "blocked":
		db.QueryRow(`SELECT COUNT(*) FROM users WHERE blocked=TRUE AND ($1='' OR username ILIKE '%'||$1||'%')`, q).Scan(&total)
	default:
		db.QueryRow(`SELECT COUNT(*) FROM users WHERE ($1='' OR username ILIKE '%'||$1||'%')`, q).Scan(&total)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"users": out, "total": total})
}

// POST /admin/user-action {"username","action":"block|unblock|logout|reset_password|delete","password":"…"}
func adminUserActionHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Action   string `json:"action"`
		Password string `json:"password"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Username == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// "create" is the only action on an account that does not exist yet.
	if req.Action != "create" && !userExists(req.Username) {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	switch req.Action {
	// The only way to create a user that depends on no external
	// infrastructure. The alternatives need a directory: LDAP/AD creates a
	// record on first sign-in, Google on first sign-in from an allowed
	// domain. Opening public registration is not an option on a corporate
	// deployment, where anyone who finds the URL would be inside.
	case "create":
		if !usernameRe.MatchString(req.Username) {
			http.Error(w, "username must be 2-32 characters: letters, digits, underscore only", http.StatusBadRequest)
			return
		}
		if isReservedUsername(req.Username) {
			http.Error(w, "username is reserved", http.StatusBadRequest)
			return
		}
		if userExists(req.Username) {
			http.Error(w, "user already exists", http.StatusConflict)
			return
		}
		// A password may be supplied, but the server generates one by
		// default: hand-made temporary passwords are predictably weak, and
		// this one only lives until the first sign-in.
		pw := strings.TrimSpace(req.Password)
		if pw == "" {
			var err error
			if pw, err = generateTempPassword(); err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
		} else if len(pw) < minPasswordLen {
			http.Error(w, fmt.Sprintf("password too short (min %d)", minPasswordLen), http.StatusBadRequest)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		// While an administrator knows the password, the account does not
		// belong to the employee; the flag clears on their first change.
		if _, err := db.Exec(
			`INSERT INTO users(username, password_hash, must_change_password) VALUES($1,$2,TRUE)`,
			req.Username, string(hash)); err != nil {
			http.Error(w, "could not create user", http.StatusInternalServerError)
			return
		}
		audit(r, "create", req.Username, "")
		// Returned once: the database holds only a bcrypt hash, so it can
		// be reset but never recovered.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "username": req.Username, "temp_password": pw,
		})
		return

	case "block":
		db.Exec(`UPDATE users SET blocked = TRUE, forced_logout_at = $2 WHERE username = $1`, req.Username, time.Now().Unix())
		setLogoutCutoff(req.Username, time.Now().Unix())
		disconnectUser(req.Username)
		audit(r, "block", req.Username, "")

	case "unblock":
		db.Exec(`UPDATE users SET blocked = FALSE WHERE username = $1`, req.Username)
		audit(r, "unblock", req.Username, "")

	case "logout":
		// Invalidate tokens and drop live connections.
		db.Exec(`UPDATE users SET forced_logout_at = $2 WHERE username = $1`, req.Username, time.Now().Unix())
		setLogoutCutoff(req.Username, time.Now().Unix())
		disconnectUser(req.Username)
		audit(r, "logout", req.Username, "")

	case "reset_password":
		if len(req.Password) < 8 {
			http.Error(w, "password too short (min 8)", http.StatusBadRequest)
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		// An admin-assigned password is known to the admin, so the account
		// is not yet the owner's; the flag clears when they change it.
		db.Exec(`UPDATE users SET password_hash = $1, must_change_password = TRUE, forced_logout_at = $3 WHERE username = $2`,
			string(hash), req.Username, time.Now().Unix())
		setLogoutCutoff(req.Username, time.Now().Unix())
		disconnectUser(req.Username)
		audit(r, "reset_password", req.Username, "")

	case "delete":
		// Collect contacts before deleting anything: direct peers first.
		var directPeers []string
		pr, _ := db.Query(
			`SELECT DISTINCT CASE WHEN sender=$1 THEN recipient ELSE sender END AS peer
			 FROM messages
			 WHERE (sender=$1 OR recipient=$1) AND recipient NOT LIKE 'g:%'`, req.Username)
		if pr != nil {
			for pr.Next() {
				var p string
				if pr.Scan(&p) == nil && p != req.Username {
					directPeers = append(directPeers, p)
				}
			}
			pr.Close()
		}
		// Groups, likewise before the group_members rows are gone.
		var userGroups []string
		gr, _ := db.Query(`SELECT group_id FROM group_members WHERE username=$1`, req.Username)
		if gr != nil {
			for gr.Next() {
				var gid string
				if gr.Scan(&gid) == nil {
					userGroups = append(userGroups, gid)
				}
			}
			gr.Close()
		}

		// Revoke the token so a deleted user cannot keep using the app on
		// an already-issued JWT.
		setLogoutCutoff(req.Username, time.Now().Unix())

		// Tell their own devices to sign out, or they sit in an endless
		// reconnect loop. Sent before disconnectUser, while sockets live.
		selfDeleted, _ := json.Marshal(map[string]string{"type": "user-deleted", "username": req.Username})
		mu.RLock()
		for _, c := range clients[req.Username] {
			c.send(selfDeleted)
		}
		mu.RUnlock()

		// Remove the conversation entirely, in both directions and with its
		// reactions, or re-creating the same username resurrects it.
		db.Exec(`DELETE FROM reactions WHERE username = $1
		         OR msg_id IN (SELECT id FROM messages WHERE sender = $1 OR recipient = $1)`, req.Username)
		db.Exec(`DELETE FROM messages WHERE sender = $1 OR recipient = $1`, req.Username)

		// Remove the account itself.
		db.Exec(`DELETE FROM group_members WHERE username = $1`, req.Username)
		db.Exec(`DELETE FROM push_subscriptions WHERE username = $1`, req.Username)
		db.Exec(`DELETE FROM users WHERE username = $1`, req.Username)
		forgetUser(req.Username)
		disconnectUser(req.Username)

		// Notify groups so members see the new membership live.
		for _, gid := range userGroups {
			invalidateGroup(gid)
			notifyGroup(gid, "group-changed")
		}

		// Tell direct peers they are offline and deleted, so their clients
		// close the conversation.
		mu.RLock()
		offlineData, _ := json.Marshal(Message{Type: "status", From: req.Username, Body: "offline"})
		deletedData, _ := json.Marshal(map[string]string{"type": "user-deleted", "username": req.Username})
		for _, peer := range directPeers {
			for _, c := range clients[peer] {
				c.send(offlineData)
				c.send(deletedData)
			}
		}
		mu.RUnlock()

		audit(r, "delete", req.Username, "messages purged")

	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// disconnectUser closes every live WS connection of a user.
func disconnectUser(username string) {
	mu.RLock()
	conns := append([]*Client(nil), clients[username]...)
	mu.RUnlock()
	for _, c := range conns {
		c.Conn.Close()
	}
}

// GET /admin/audit?limit=&q=&action= — searchable action log.
func adminAuditHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	limit := 100
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := "%" + strings.TrimSpace(r.URL.Query().Get("q")) + "%"
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	// Without OFFSET the panel can only ever show the first page, which
	// makes the log useless at any real volume.
	offset := 0
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
	if offset < 0 {
		offset = 0
	}
	var rows *sql.Rows
	var err error
	if action != "" {
		rows, err = db.Query(`
			SELECT action, target, details, ip, EXTRACT(EPOCH FROM created_at)*1000
			FROM admin_audit WHERE action=$1 AND target ILIKE $2
			ORDER BY created_at DESC LIMIT $3 OFFSET $4`, action, q, limit, offset)
	} else {
		rows, err = db.Query(`
			SELECT action, target, details, ip, EXTRACT(EPOCH FROM created_at)*1000
			FROM admin_audit WHERE target ILIKE $1
			ORDER BY created_at DESC LIMIT $2 OFFSET $3`, q, limit, offset)
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type entry struct {
		Action  string `json:"action"`
		Target  string `json:"target"`
		Details string `json:"details"`
		IP      string `json:"ip"`
		At      int64  `json:"at"`
	}
	out := []entry{}
	for rows.Next() {
		var e entry
		var at float64
		if rows.Scan(&e.Action, &e.Target, &e.Details, &e.IP, &at) == nil {
			e.At = int64(at)
			out = append(out, e)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type adminGroup struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedBy string `json:"created_by"`
	Members   int    `json:"members"`
	Messages  int    `json:"messages"`
	CreatedAt int64  `json:"created_at"`
}

// GET /admin/groups?q=&limit=&offset=
func adminGroupsHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := 50, 0
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.Query(`
		SELECT g.id, g.name, g.created_by,
		       EXTRACT(EPOCH FROM g.created_at)*1000,
		       (SELECT COUNT(*) FROM group_members gm WHERE gm.group_id = g.id),
		       (SELECT COUNT(*) FROM messages m WHERE m.recipient = g.id)
		FROM groups g
		WHERE ($1 = '' OR g.name ILIKE '%' || $1 || '%')
		ORDER BY g.created_at DESC
		LIMIT $2 OFFSET $3`, q, limit, offset)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []adminGroup{}
	for rows.Next() {
		var g adminGroup
		var at float64
		if rows.Scan(&g.ID, &g.Name, &g.CreatedBy, &at, &g.Members, &g.Messages) != nil {
			continue
		}
		g.CreatedAt = int64(at)
		out = append(out, g)
	}
	var total int
	db.QueryRow(`SELECT COUNT(*) FROM groups WHERE ($1 = '' OR name ILIKE '%' || $1 || '%')`, q).Scan(&total)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"groups": out, "total": total})
}

// GET /admin/group-members?group_id=
func adminGroupMembersHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	gid := r.URL.Query().Get("group_id")
	if gid == "" {
		http.Error(w, "group_id required", http.StatusBadRequest)
		return
	}
	rows, err := db.Query(`SELECT username, role FROM group_members WHERE group_id=$1 ORDER BY role DESC, username`, gid)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type member struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	out := []member{}
	for rows.Next() {
		var m member
		if rows.Scan(&m.Username, &m.Role) == nil {
			out = append(out, m)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// POST /admin/group-action {"group_id","action":"delete|remove_member","username":"…"}
func adminGroupActionHandler(w http.ResponseWriter, r *http.Request) {
	// The guard runs first and on its own: folding it into the method
	// check writes a 405 on top of the response the guard already sent for
	// an OPTIONS preflight.
	if !adminGuard(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		GroupID  string `json:"group_id"`
		Action   string `json:"action"`
		Username string `json:"username"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.GroupID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "delete":
		db.Exec(`DELETE FROM group_members WHERE group_id=$1`, req.GroupID)
		db.Exec(`DELETE FROM groups WHERE id=$1`, req.GroupID)
		invalidateGroup(req.GroupID)
		notifyGroup(req.GroupID, "group-changed")
		audit(r, "delete_group", req.GroupID, "")
	case "remove_member":
		if req.Username == "" {
			http.Error(w, "username required", http.StatusBadRequest)
			return
		}
		db.Exec(`DELETE FROM group_members WHERE group_id=$1 AND username=$2`, req.GroupID, req.Username)
		invalidateGroup(req.GroupID)
		notifyGroup(req.GroupID, "group-changed")
		audit(r, "remove_member", req.GroupID, req.Username)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// userBlocked is checked at sign-in and on every WS connect. It is
// deliberately uncached: a block must take effect immediately.
func userBlocked(username string) bool {
	var blocked bool
	err := db.QueryRow(`SELECT blocked FROM users WHERE username = $1`, username).Scan(&blocked)
	if err == sql.ErrNoRows {
		// No row means the user was deleted. Refuse: otherwise a deleted
		// account with a still-valid JWT reconnects and keeps writing.
		return true
	}
	if err != nil {
		// Any other database error fails open rather than locking
		// everyone out at once.
		return false
	}
	return blocked
}

// validateToken runs on every request, so the forced-logout mark cannot be
// read from the database each time. The cache is warmed at startup and
// updated on every admin action.

var logoutCutoffs sync.Map // username -> int64 unix time of the revocation

func setLogoutCutoff(username string, at int64) {
	logoutCutoffs.Store(username, at)
}

func logoutCutoff(username string) (int64, bool) {
	v, ok := logoutCutoffs.Load(username)
	if !ok {
		return 0, false
	}
	at, ok := v.(int64)
	return at, ok
}

// loadLogoutCutoffs warms the cache at startup; without it a restart brings
// revoked tokens back to life.
func loadLogoutCutoffs() {
	rows, err := db.Query(`SELECT username, forced_logout_at FROM users WHERE forced_logout_at > 0`)
	if err != nil {
		// Failing silently here means previously revoked tokens are
		// accepted again with nothing in the log to say so. The service
		// still starts without the cache, but the fact must be visible.
		log.Printf("WARNING: could not load forced-logout marks (%v) — previously revoked tokens will be accepted", err)
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var u string
		var at int64
		if rows.Scan(&u, &at) == nil {
			logoutCutoffs.Store(u, at)
			n++
		}
	}
	if n > 0 {
		log.Printf("loaded %d forced-logout markers", n)
	}
}
