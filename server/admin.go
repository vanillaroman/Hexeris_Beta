package main

// The admin panel: user management and an audit trail of actions.
// Part of the Hexeris server; the shared types and main() live in main.go.
//
// The panel is static and lives on a different host (admin.example.com), so
// every endpoint sends CORS. The key is accepted in the X-Admin-Key header —
// in a query string it would show up in logs and the browser's history.

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

// adminAllowedIPs is the second line after the key: even a leaked ADMIN_KEY is
// useless from a foreign address. Set through ADMIN_ALLOWED_IPS (a
// comma-separated list). While the variable is empty the filter does NOT
// block but logs the real addresses — so you can see what to put in without
// risking locking yourself out.
//
// Important: if the panel is static and the browser calls the API directly,
// what arrives here is the administrator's own IP, not the panel host's.
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

// keyFingerprint — eight hex characters of SHA-256, so keys can be compared in
// the log without printing them. An empty key is marked with a word rather
// than the hash of an empty string: "there was no header at all" is its own
// diagnosis and must not look like "some key arrived".
//
// The label goes into the server log, which is read by the customer's
// operator, so it stays short and plain.
func keyFingerprint(s string) string {
	if s == "" {
		return "(empty)"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// adminGuard sets CORS, answers preflight and checks the key.
// Returns false when the request must not be processed any further.
func adminGuard(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", cfg.AdminOrigin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "X-Admin-Key, Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	// The header is the main way; the query is kept for compatibility with the old
	// panel version, but it should go once that is updated.
	key := r.Header.Get("X-Admin-Key")
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	if subtle.ConstantTimeCompare([]byte(key), []byte(adminKey())) != 1 {
		// The answer says nothing specific: whoever does not know the key has no
		// business knowing why. The log, however, gets a line: before this a wrong
		// key was not logged at all, and a "403" seen from the panel was
		// indistinguishable from a "403" caused by the IP filter.
		//
		// FINGERPRINTS go into the log, not the keys themselves: the log is read more
		// widely than the config, and one day someone who should not know the key will
		// look into it "for debugging". Eight hex characters of SHA-256 reveal nothing
		// (the key is 256 bits from crypto/rand) but are enough to answer the only
		// question that arises here: is this the same key or two different ones.
		// Equal lengths are not enough — two keys generated with
		// "openssl rand -hex 32" one after the other are both 64 characters long and
		// both wrong.
		log.Printf("ADMIN BLOCKED from %s: X-Admin-Key does not match "+
			"(sent len=%d sha=%s, expected len=%d sha=%s)",
			getIP(r), len(key), keyFingerprint(key), len(adminKey()), keyFingerprint(adminKey()))
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	// The key has already been checked, so here we can be specific: only someone
	// who already has the key sees the hint, and it saves the operator an hour of
	// telling two identical-looking refusals apart.
	if !adminIPAllowed(r) {
		http.Error(w, "forbidden: source IP "+getIP(r)+" is not in ADMIN_ALLOWED_IPS",
			http.StatusForbidden)
		return false
	}
	return true
}

// adminUnknownEndpointMarker — the panel uses this substring to tell "the
// messenger answered but knows no such endpoint" from "nginx returned the 404
// without reaching the proxy location". They are fixed on different machines
// and share a status code. A test compares this string with
// docs/admin-panel/admin-index.html: if they drift apart, the panel returns
// exactly the wrong hint this was all written to get away from.
const adminUnknownEndpointMarker = "unknown admin endpoint"

// adminUnknownHandler answers /admin/* requests with no handler of their own.
//
// Without it such a request fell through to the "/" catch-all and got the
// MESSENGER's index.html with a 200: the panel took the request for a success
// and then failed while parsing JSON, and a 404 from the server never arose.
// The answer is served behind adminGuard — an unknown source has no business
// receiving an enumerator of existing endpoints.
func adminUnknownHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	http.Error(w, adminUnknownEndpointMarker+": "+r.URL.Path, http.StatusNotFound)
}

// adminAPIAliasPrefix — the prefix the panel on admin.example.com uses to reach
// the admin API. The messenger accepts it alongside /admin/.
const adminAPIAliasPrefix = "/admin-api/"

// adminAPIAliasHandler accepts /admin-api/* and serves it as /admin/*.
//
// Why the server knows about a foreign prefix. nginx on the admin host used to
// rewrite it — by substituting a capture group in proxy_pass, then in rewrite.
// In production that substitution drifted: $1 gave data from the START of the
// URI at the right length, /admin//admin- went upstream, and the whole panel
// got 404s (the investigation is in docs/engineering/ADMIN_404.md). It could
// have been worked around in nginx, but any such workaround again rests on
// either a capture group or a static backend name — and our name is dynamic
// (DDNS), so a static one would eat the address re-resolution.
//
// Accepting the prefix here removes path rewriting from the proxy entirely:
// there is nothing to rewrite, so nothing to break. The configuration on that
// side collapses to a proxy_pass with no URI, where the path goes as it is.
//
// This opens no new surface: the handlers behind the prefix are the same ones
// and each begins with adminGuard.
func adminAPIAliasHandler(w http.ResponseWriter, r *http.Request) {
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/admin/" + strings.TrimPrefix(r.URL.Path, adminAPIAliasPrefix)
	// Path has been rewritten — the raw variant no longer matches it, and leaving
	// it would make EscapedPath return the old path.
	r2.URL.RawPath = ""
	// TrimPrefix guarantees the new path starts with /admin/ rather than
	// /admin-api/ — the mux cannot loop on itself.
	http.DefaultServeMux.ServeHTTP(w, r2)
}

// initAdminSchema creates what only the admin panel needs.
func initAdminSchema() {
	initLoginAuditSchema()
	// Blocking instead of deleting: a blocked user's messages stay in their peers'
	// history, but they cannot sign in.
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS blocked BOOLEAN NOT NULL DEFAULT FALSE`)
	// forced_logout_at invalidates issued JWTs: tokens with an iat no later than
	// this stamp stop passing validation (see validateToken).
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

// audit records an administrative action. Without it there is no answering
// "who blocked this user, and when".
func audit(r *http.Request, action, target, details string) {
	// For the vast majority of actions a failed log write is no reason to cancel
	// the action itself: blocking a user cannot be rolled back because the audit
	// table is unavailable. There is one exception — exporting someone else's
	// correspondence — and it uses auditErr directly.
	_ = auditErr(r, action, target, details)
}

// auditErr — the same, but it reports whether the write succeeded. Needed
// where an unrecorded action is inadmissible in itself.
func auditErr(r *http.Request, action, target, details string) error {
	_, err := db.Exec(`INSERT INTO admin_audit(action, target, details, ip) VALUES($1,$2,$3,$4)`,
		action, target, details, getIP(r))
	if err != nil {
		log.Printf("ADMIN AUDIT FAILED %s target=%s: %v", action, target, err)
	}
	log.Printf("ADMIN %s target=%s %s from=%s", action, target, details, getIP(r))
	return err
}

// ─── Users ───────────────────────────────────────────────────────────────────

type adminUser struct {
	Username string `json:"username"`
	// The profile fields. The columns were in users from the start (the migrations
	// are in main.go), but this endpoint did not select them — Display name /
	// Position / Email were empty for everyone in the panel and in the CSV,
	// filled-in accounts included, and the phone was not there at all.
	DisplayName string `json:"display_name"`
	Position    string `json:"position"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	CreatedAt   int64  `json:"created_at"`
	Blocked     bool   `json:"blocked"`
	Online      bool   `json:"online"`
	Messages    int    `json:"messages"`
	LastSeen    int64  `json:"last_seen"`
	// Whether an employee has a second factor. An administrator needs this to
	// answer two questions: who lacks one, and whose to reset.
	TwoFA bool `json:"totp_enabled"`
}

// adminUserCols — one column list for every filter branch. The SELECT used to
// be copied three times: adding a field needed three synchronised edits, and a
// divergence would show as an empty column under one filter only.
// The order must match scanAdminUser below.
const adminUserCols = `
			u.username, u.display_name, u.position, u.email, u.phone,
			EXTRACT(EPOCH FROM u.created_at)*1000, u.blocked,
			(SELECT COUNT(*) FROM messages m WHERE m.sender=u.username),
			COALESCE((SELECT EXTRACT(EPOCH FROM MAX(m.created_at))*1000 FROM messages m WHERE m.sender=u.username),0),
			COALESCE(u.totp_enabled,false)`

func scanAdminUser(rows *sql.Rows) (adminUser, error) {
	var u adminUser
	var created, last float64
	err := rows.Scan(&u.Username, &u.DisplayName, &u.Position, &u.Email, &u.Phone,
		&created, &u.Blocked, &u.Messages, &last, &u.TwoFA)
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

	// The message counter and last activity come from subqueries so the whole
	// messages table is not pulled into memory.
	filter := r.URL.Query().Get("filter") // online | blocked | ""
	var rows *sql.Rows
	var err error
	// For the online filter the list comes from clients (in memory) and is passed
	// to SQL through ANY($) — otherwise LIMIT would cut users before filtering.
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
			// Nobody online — an empty answer right away
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
	// "create" is the only action on an account that does NOT yet exist.
	if req.Action != "create" && !userExists(req.Username) {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	switch req.Action {
	// The only way to create a user that does not depend on the customer's
	// infrastructure. The other two need a directory: LDAP/AD creates a record on
	// first sign-in, Google on first sign-in from an allowed domain. Opening public
	// registration on a corporate instance is not an option: anyone who found the
	// URL would be inside the work chat.
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
		// The password can be set explicitly, but by default the server generates it:
		// hand-invented temporary passwords are predictably weak, and this one lives
		// only until the first sign-in anyway.
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
		// must_change_password: while the administrator knows the password, the
		// account does not belong to the employee. Cleared on the first change.
		if _, err := db.Exec(
			`INSERT INTO users(username, password_hash, must_change_password) VALUES($1,$2,TRUE)`,
			req.Username, string(hash)); err != nil {
			http.Error(w, "could not create user", http.StatusInternalServerError)
			return
		}
		audit(r, "create", req.Username, "")
		// The password is returned ONCE — the database holds only a bcrypt hash and
		// it cannot be recovered afterwards, only reset again.
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
		// Invalidate the tokens and tear down live connections.
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
		// must_change_password: a password set by an administrator is known to them —
		// such an account does not belong to its owner yet. The flag clears when the
		// user changes the password themselves.
		db.Exec(`UPDATE users SET password_hash = $1, must_change_password = TRUE, forced_logout_at = $3 WHERE username = $2`,
			string(hash), req.Username, time.Now().Unix())
		setLogoutCutoff(req.Username, time.Now().Unix())
		disconnectUser(req.Username)
		audit(r, "reset_password", req.Username, "")

	case "reset_2fa":
		// The employee lost both their phone and their recovery codes — otherwise they
		// would have managed alone. Without this branch the only way out would be to
		// delete the account along with the correspondence.
		//
		// The action removes protection, so it also tears down sessions: otherwise
		// whoever obtained the reset gets in with a password alone and the owner never
		// finds out. And it is, of course, in the log.
		if err := twoFAReset(req.Username); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		db.Exec(`UPDATE users SET forced_logout_at = $2 WHERE username = $1`, req.Username, time.Now().Unix())
		setLogoutCutoff(req.Username, time.Now().Unix())
		disconnectUser(req.Username)
		audit(r, "reset_2fa", req.Username, "two-factor removed by an administrator")

	case "delete":
		// 1. Collect the contacts BEFORE deleting from the database.
		// Direct peers: those there was a direct conversation with.
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
		// Groups: collected before deleting from group_members.
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

		// 2. Revoke the token: a deleted user must not keep using the application
		//    with an already issued JWT (checked in validateToken through
		//    logoutCutoff, for both WS and HTTP endpoints).
		setLogoutCutoff(req.Username, time.Now().Unix())

		// 3. Tell the deleted user on THEIR devices so the client signs out
		//    (otherwise it would hang in an endless reconnect). Sent before
		//    disconnectUser, while the sockets are still alive.
		selfDeleted, _ := json.Marshal(map[string]string{"type": "user-deleted", "username": req.Username})
		mu.RLock()
		for _, c := range clients[req.Username] {
			c.send(selfDeleted)
		}
		mu.RUnlock()

		// 4. Delete the correspondence entirely — their messages and those addressed
		//    to them, reactions included. Otherwise recreating the same name would
		//    resurface the old history.
		db.Exec(`DELETE FROM reactions WHERE username = $1
		         OR msg_id IN (SELECT id FROM messages WHERE sender = $1 OR recipient = $1)`, req.Username)
		db.Exec(`DELETE FROM messages WHERE sender = $1 OR recipient = $1`, req.Username)

		// 5. Delete the account. chat_prefs is cleared BOTH ways: their preferences
		//    for other conversations and other people's preferences for the
		//    conversation with them. The second half matters more — without it
		//    "muted" and "archived" set for that name would survive the deletion
		//    and be inherited by a new person if the name is created again.
		db.Exec(`DELETE FROM chat_prefs WHERE username = $1 OR peer = $1`, req.Username)
		db.Exec(`DELETE FROM group_members WHERE username = $1`, req.Username)
		db.Exec(`DELETE FROM push_subscriptions WHERE username = $1`, req.Username)
		db.Exec(`DELETE FROM users WHERE username = $1`, req.Username)
		forgetUser(req.Username)
		disconnectUser(req.Username)

		// 6. Notify the groups — members see the updated membership live.
		for _, gid := range userGroups {
			invalidateGroup(gid)
			notifyGroup(gid, "group-changed")
		}

		// 7. Broadcast an offline status to direct peers plus user-deleted so their
		//    client closes the conversation with the deleted user.
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

// disconnectUser closes all of a user's live WS connections.
func disconnectUser(username string) {
	mu.RLock()
	conns := append([]*Client(nil), clients[username]...)
	mu.RUnlock()
	for _, c := range conns {
		c.Conn.Close()
	}
}

// GET /admin/audit?limit=&q=&action= — the log, with search.
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
	// OFFSET: without it the panel showed only the first page and the rest was
	// unreachable — at real volumes the log became useless.
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

// ─── Groups ──────────────────────────────────────────────────────────────────

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
	// The guard strictly first and on its own: the old combined condition wrote a
	// 405 OVER the answer the guard had already sent to an OPTIONS preflight
	// (superfluous WriteHeader) and to any non-POST without a key.
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

// ─── The block check ─────────────────────────────────────────────────────────

// userBlocked is checked at sign-in and on WS connection.
// No cache is used: a block must take effect immediately.
func userBlocked(username string) bool {
	var blocked bool
	err := db.QueryRow(`SELECT blocked FROM users WHERE username = $1`, username).Scan(&blocked)
	if err == sql.ErrNoRows {
		// No row — the user is deleted (or never existed). Refused: otherwise a
		// deleted account with a still-valid JWT would reconnect its WS and keep
		// writing. Checked at sign-in and on every WS connection.
		return true
	}
	if err != nil {
		// Any other database error — do not block everyone at once (fail-open).
		return false
	}
	return blocked
}

// ─── The forced-logout cache ─────────────────────────────────────────────────
// validateToken runs on every request, so the stamp cannot be checked in the
// database — it is kept in memory. The cache is warmed at start-up and
// refreshed on every administrator action.

var logoutCutoffs sync.Map // username -> int64 (the unix time of the reset)

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

// loadLogoutCutoffs warms the cache at start-up: without it, revoked tokens
// would start working again after a server restart.
func loadLogoutCutoffs() {
	rows, err := db.Query(`SELECT username, forced_logout_at FROM users WHERE forced_logout_at > 0`)
	if err != nil {
		// Silence here is dangerous: without a warm cache, previously revoked
		// tokens count as valid again, and nothing in the log said so. We do not
		// start dying — the service comes up without the cache — but the fact must
		// be visible to the operator.
		log.Printf("WARNING: could not load the forced-logout stamps (%v) — previously revoked tokens will be accepted", err)
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
