package main

// Sign-in audit: who signed in, from where, when — and who failed.
//
// Separate from admin_audit, which records administrator actions. A security
// team's first question is "who signed in and when", and failed attempts also
// explain rate-limiter blocks that would otherwise look like events without
// a cause (see docs/SECURITY.md on shared NAT addresses).
//
// Rows contain a username and an IP address, which is personal data, so
// retention is bounded by LOGIN_AUDIT_KEEP_DAYS (90 by default) rather than
// kept indefinitely.

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Sign-in outcomes. These are machine tokens; the admin panel renders the
// human-readable text.
const (
	loginOK        = "ok"
	loginBadCreds  = "bad_credentials"
	loginBlocked   = "blocked"
	loginRateLimit = "rate_limited"
)

func initLoginAuditSchema() {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS login_audit (
			id         BIGSERIAL PRIMARY KEY,
			username   TEXT NOT NULL,
			outcome    TEXT NOT NULL,
			method     TEXT NOT NULL DEFAULT 'password',
			ip         TEXT,
			user_agent TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`); err != nil {
		log.Println("login_audit schema:", err)
		return
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_login_audit_created ON login_audit(created_at DESC)`)
	// "Show every sign-in by this employee" is the routine security query,
	// hence a dedicated index.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_login_audit_user ON login_audit(username, created_at DESC)`)
}

// recordLogin never fails the handler: an audit write is not a reason to
// refuse someone entry, so errors only go to the log.
func recordLogin(r *http.Request, username, outcome, method string) {
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	if _, err := db.Exec(
		`INSERT INTO login_audit(username, outcome, method, ip, user_agent) VALUES($1,$2,$3,$4,$5)`,
		username, outcome, method, getIP(r), ua); err != nil {
		log.Println("login audit write failed:", err)
	}
}

func adminLoginAuditHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	limit := 100
	fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := "%" + strings.TrimSpace(r.URL.Query().Get("q")) + "%"
	outcome := strings.TrimSpace(r.URL.Query().Get("outcome"))
	offset := 0
	fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
	if offset < 0 {
		offset = 0
	}

	var rows *sql.Rows
	var err error
	if outcome != "" {
		rows, err = db.Query(`
			SELECT username, outcome, method, COALESCE(ip,''), COALESCE(user_agent,''),
			       EXTRACT(EPOCH FROM created_at)*1000
			FROM login_audit WHERE outcome=$1 AND username ILIKE $2
			ORDER BY created_at DESC LIMIT $3 OFFSET $4`, outcome, q, limit, offset)
	} else {
		rows, err = db.Query(`
			SELECT username, outcome, method, COALESCE(ip,''), COALESCE(user_agent,''),
			       EXTRACT(EPOCH FROM created_at)*1000
			FROM login_audit WHERE username ILIKE $1
			ORDER BY created_at DESC LIMIT $2 OFFSET $3`, q, limit, offset)
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type entry struct {
		Username  string `json:"username"`
		Outcome   string `json:"outcome"`
		Method    string `json:"method"`
		IP        string `json:"ip"`
		UserAgent string `json:"user_agent"`
		CreatedAt int64  `json:"created_at"`
	}
	out := []entry{}
	for rows.Next() {
		var e entry
		var ts float64
		if rows.Scan(&e.Username, &e.Outcome, &e.Method, &e.IP, &e.UserAgent, &ts) == nil {
			e.CreatedAt = int64(ts)
			out = append(out, e)
		}
	}
	writeJSON(w, out)
}

// pruneLoginAudit bounds retention: a username plus an IP address is personal
// data, and keeping it forever contradicts data minimisation.
func pruneLoginAudit(days int) {
	if days <= 0 {
		return
	}
	res, err := db.Exec(
		`DELETE FROM login_audit WHERE created_at < NOW() - ($1 || ' days')::interval`, days)
	if err != nil {
		log.Println("login audit prune:", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("login audit: removed %d entries older than %d days", n, days)
	}
}

// startLoginAuditJanitor runs regardless of RETENTION_ENABLED.
//
// General retention is the operator's decision about content: it is switched
// off deliberately because conversations are meant to be kept. Sign-in audit
// is not content but operational personal data, which should not accumulate
// indefinitely even where content retention is off. Hence its own schedule
// and LOGIN_AUDIT_KEEP_DAYS (0 disables pruning as an explicit choice).
func startLoginAuditJanitor() {
	days := getEnvInt("LOGIN_AUDIT_KEEP_DAYS", 90)
	if days <= 0 {
		log.Println("login audit retention DISABLED (LOGIN_AUDIT_KEEP_DAYS=0)")
		return
	}
	log.Printf("login audit retention: %d days", days)
	safeGo("loginAuditJanitor", func() {
		pruneLoginAudit(days) // catch-up pass at startup
		for range time.Tick(24 * time.Hour) {
			pruneLoginAudit(days)
		}
	})
}
