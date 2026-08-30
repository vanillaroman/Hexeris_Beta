package main

// The second factor for local password sign-in: storage, enabling, the check
// at sign-in, and recovering access.
//
// The algorithm lives in totp.go; everything else is here.
//
// ═══ WHAT IS PROTECTED, AND FROM WHAT ═════════════════════════════════════
//
// From a stolen password: leaked from another service, glimpsed, phished. It
// is not protection from someone who already has access to the server — from
// them nothing on the server protects (docs/security/SECURITY.md).
//
// ═══ DECISIONS THAT ARE EASY TO GET WRONG ═════════════════════════════════
//
// **The secret is stored encrypted.** With the same SERVER_ENC_KEY as message
// bodies. A TOTP secret IS the whole second factor: whoever reads it from the
// database generates codes themselves. Keeping it next to the bcrypt password
// hash in the clear would mean a database dump cancels the second factor,
// while the passwords in that same dump survive.
//
// **A code cannot be used twice.** The number of the last accepted window is
// stored in the database, and a code from the same or an earlier window is
// rejected. Without that a code read over your shoulder works for another
// minute and a half — and the tolerance window (totpSkew) opens it.
//
// **Between the password and the code stands a single-use ticket, not a
// token.** Otherwise a working token would have to be issued before the second
// factor ("we will check later") — that is, to have no second factor at all.
// The ticket lives five minutes, remembers the attempt count and grants
//
// nothing but the right to present a code.
// **Enabling requires a code, not merely a button press.** Until the person
// has proved their app really reads the secret there is nothing to enable:
// otherwise the first result is an employee locked out by a bad scan.
//
// **Turning it off requires the password AND a code.** That cancels the
// protection, and doing it with one click from an already open session reduces
// the protection to "while the laptop is unlocked".

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

const (
	twoFATicketTTL      = 5 * time.Minute
	twoFATicketAttempts = 5
	twoFARecoveryCount  = 10
)

func initTwoFASchema() {
	// The columns are always created even if nobody enables the second factor:
	// empty columns cost nothing, a migration on a live server does.
	for _, q := range []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_secret TEXT`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_enabled BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_step BIGINT NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(q); err != nil {
			log.Println("2fa schema:", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS twofa_recovery (
		username   TEXT NOT NULL,
		code_hash  TEXT NOT NULL,
		used_at    TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (username, code_hash)
	)`); err != nil {
		log.Println("2fa recovery schema:", err)
	}
}

// ─── The ticket between password and code ────────────────────────────────

type twoFATicket struct {
	username string
	expires  time.Time
	attempts int
}

var (
	twoFAMu      sync.Mutex
	twoFATickets = map[string]*twoFATicket{}
)

func twoFAIssueTicket(username string) (string, error) {
	id, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}
	twoFAMu.Lock()
	defer twoFAMu.Unlock()
	// Cleanup along the way: there are few tickets and they live five minutes, so
	// a separate collector would be one more moving part.
	now := time.Now()
	for k, v := range twoFATickets {
		if now.After(v.expires) {
			delete(twoFATickets, k)
		}
	}
	twoFATickets[id] = &twoFATicket{username: username, expires: now.Add(twoFATicketTTL)}
	return id, nil
}

// twoFAClaimAttempt returns the ticket owner and spends one attempt. The
// ticket disappears when the attempts run out: otherwise it becomes a way to
// guess a million codes knowing only the password.
func twoFAClaimAttempt(id string) (string, bool) {
	twoFAMu.Lock()
	defer twoFAMu.Unlock()
	t, ok := twoFATickets[id]
	if !ok || time.Now().After(t.expires) {
		delete(twoFATickets, id)
		return "", false
	}
	t.attempts++
	if t.attempts > twoFATicketAttempts {
		delete(twoFATickets, id)
		return "", false
	}
	return t.username, true
}

func twoFADropTicket(id string) {
	twoFAMu.Lock()
	delete(twoFATickets, id)
	twoFAMu.Unlock()
}

// ─── Storing the secret ──────────────────────────────────────────────────

// twoFASecret returns a user's secret and whether the second factor is on.
func twoFASecret(username string) (secret string, enabled bool, err error) {
	var stored sql.NullString
	err = db.QueryRow(`SELECT totp_secret, totp_enabled FROM users WHERE username=$1`,
		username).Scan(&stored, &enabled)
	if err != nil {
		return "", false, err
	}
	if !stored.Valid || stored.String == "" {
		return "", enabled, nil
	}
	return decryptBody(stored.String), enabled, nil
}

func twoFAEnabled(username string) bool {
	var on bool
	if err := db.QueryRow(`SELECT COALESCE(totp_enabled,false) FROM users WHERE username=$1`,
		username).Scan(&on); err != nil {
		return false
	}
	return on
}

// twoFAConsume checks a code and guards against reuse.
//
// The check and the write are one UPDATE with a condition on totp_last_step:
// two parallel attempts with the same code would otherwise both pass, because
// the second slips in between the read and the write.
func twoFAConsume(username, code string) bool {
	secret, enabled, err := twoFASecret(username)
	if err != nil || secret == "" || !enabled {
		return false
	}
	step, ok := totpVerify(secret, code, time.Now())
	if !ok {
		return false
	}
	res, err := db.Exec(
		`UPDATE users SET totp_last_step=$2 WHERE username=$1 AND totp_last_step < $2`,
		username, step)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// This window is already used — the code was presented twice.
		log.Printf("2fa: code reuse rejected, user=%q", username)
		return false
	}
	return true
}

// ─── Recovery codes ──────────────────────────────────────────────────────

// twoFAHashRecovery uses sha256 rather than bcrypt, deliberately. A recovery
// code is generated by us and carries 50 bits of randomness, so guessing it is
// pointless regardless of hash speed. Bcrypt would mean ten of its
// computations per sign-in attempt — a second of CPU time per request, which
// is an easy way to take the server down.
func twoFAHashRecovery(code string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

var recoveryAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

func twoFANewRecoveryCodes(username string) ([]string, error) {
	codes := make([]string, 0, twoFARecoveryCount)
	for len(codes) < twoFARecoveryCount {
		b := make([]byte, 10) // 80 bits -> 16 base32 characters
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		s := recoveryAlphabet.EncodeToString(b)
		codes = append(codes, s[:4]+"-"+s[4:8]+"-"+s[8:12]+"-"+s[12:16])
	}
	// Reissuing invalidates the old codes: the list printed last time must not
	// work after a reissue.
	if _, err := db.Exec(`DELETE FROM twofa_recovery WHERE username=$1`, username); err != nil {
		return nil, err
	}
	for _, c := range codes {
		if _, err := db.Exec(`INSERT INTO twofa_recovery(username, code_hash) VALUES($1,$2)`,
			username, twoFAHashRecovery(c)); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// twoFAUseRecovery spends a recovery code. Single-use: the row is marked used
// by the same query that finds it.
func twoFAUseRecovery(username, code string) bool {
	res, err := db.Exec(
		`UPDATE twofa_recovery SET used_at=NOW() WHERE username=$1 AND code_hash=$2 AND used_at IS NULL`,
		username, twoFAHashRecovery(code))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		// This event must be visible: a recovery code is used either when a phone
		// is lost or when access is already in the wrong hands.
		log.Printf("2fa: SIGN-IN WITH A RECOVERY CODE, user=%q", username)
	}
	return n == 1
}

func twoFARecoveryLeft(username string) int {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM twofa_recovery WHERE username=$1 AND used_at IS NULL`,
		username).Scan(&n)
	return n
}

// twoFAReset removes the second factor entirely. Used by an administrator when
// an employee has lost both their phone and their recovery codes.
func twoFAReset(username string) error {
	if _, err := db.Exec(
		`UPDATE users SET totp_secret=NULL, totp_enabled=FALSE, totp_last_step=0 WHERE username=$1`,
		username); err != nil {
		return err
	}
	_, err := db.Exec(`DELETE FROM twofa_recovery WHERE username=$1`, username)
	return err
}

// ─── Endpoints ───────────────────────────────────────────────────────────

func twoFAStatusHandler(w http.ResponseWriter, r *http.Request) {
	me, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{
		"enabled":       twoFAEnabled(me),
		"recovery_left": twoFARecoveryLeft(me),
		"issuer":        twoFAIssuer(),
	})
}

// twoFASetupHandler issues a new secret and an image for the app. The second
// factor is NOT yet enabled — not until a code confirms it.
func twoFASetupHandler(w http.ResponseWriter, r *http.Request) {
	me, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if twoFAEnabled(me) {
		// Reissuing the secret while the factor is on is a substitution, and
		// doing it without turning it off first is not allowed: otherwise one
		// open session is enough to move the second factor to someone else's phone.
		http.Error(w, "two-factor authentication is already on — turn it off first", http.StatusConflict)
		return
	}
	secret, err := totpNewSecret()
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if _, err := db.Exec(`UPDATE users SET totp_secret=$2, totp_enabled=FALSE WHERE username=$1`,
		me, encryptBody(secret)); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	uri := totpURI(twoFAIssuer(), me, secret)
	png, err := qrcode.Encode(uri, qrcode.Medium, 320)
	if err != nil {
		// The image did not render — but entering the secret by hand still
		// works, and refusing outright would be worse.
		log.Println("2fa: could not build the QR code:", err)
	}
	writeJSON(w, map[string]any{
		"secret": secret,
		"uri":    uri,
		"qr_png": pngDataURI(png),
	})
}

func twoFAEnableHandler(w http.ResponseWriter, r *http.Request) {
	me, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	secret, enabled, err := twoFASecret(me)
	if err != nil || secret == "" {
		http.Error(w, "start the setup first", http.StatusBadRequest)
		return
	}
	if enabled {
		http.Error(w, "two-factor authentication is already on", http.StatusConflict)
		return
	}
	step, valid := totpVerify(secret, req.Code, time.Now())
	if !valid {
		http.Error(w, "that code does not match — check the time on your phone and try the next one",
			http.StatusUnauthorized)
		return
	}
	if _, err := db.Exec(`UPDATE users SET totp_enabled=TRUE, totp_last_step=$2 WHERE username=$1`,
		me, step); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	codes, err := twoFANewRecoveryCodes(me)
	if err != nil {
		log.Println("2fa: could not issue recovery codes:", err)
	}
	log.Printf("2fa: enabled for %q", me)
	// The codes are shown exactly once: keeping them readable on our side would
	// mean keeping a spare key to the database next to the database.
	writeJSON(w, map[string]any{"enabled": true, "recovery_codes": codes})
}

func twoFADisableHandler(w http.ResponseWriter, r *http.Request) {
	me, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	// The password, because an open session does not prove the owner is at the
	// laptop. The code, because otherwise a stolen password is enough again.
	var hash string
	if err := db.QueryRow(`SELECT password_hash FROM users WHERE username=$1`, me).Scan(&hash); err != nil ||
		bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		http.Error(w, "wrong password", http.StatusUnauthorized)
		return
	}
	if !twoFAConsume(me, req.Code) && !twoFAUseRecovery(me, req.Code) {
		http.Error(w, "wrong code", http.StatusUnauthorized)
		return
	}
	if err := twoFAReset(me); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	log.Printf("2fa: disabled for %q", me)
	writeJSON(w, map[string]any{"enabled": false})
}

// twoFAVerifyHandler is the second sign-in step. The ticket becomes a token.
func twoFAVerifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := getIP(r)
	if loginLimiter.isBlocked(ip) {
		http.Error(w, "too many attempts, try again in 10 minutes", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
		Code   string `json:"code"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	username, ok := twoFAClaimAttempt(req.Ticket)
	if !ok {
		// One answer for both an expired ticket and exhausted attempts: the
		// difference between them would hint at whether to keep trying.
		http.Error(w, "this sign-in attempt has expired — enter your password again", http.StatusUnauthorized)
		return
	}

	usedRecovery := false
	if !twoFAConsume(username, req.Code) {
		if !twoFAUseRecovery(username, req.Code) {
			loginLimiter.recordFailure(ip)
			log.Printf("2fa FAILED: user=%q ip=%s", username, ip)
			recordLogin(r, username, login2FABad, "password+totp")
			http.Error(w, "wrong code", http.StatusUnauthorized)
			return
		}
		usedRecovery = true
	}
	twoFADropTicket(req.Ticket)

	// The block may have happened while the person looked for their phone.
	if userBlocked(username) {
		recordLogin(r, username, loginBlocked, "password+totp")
		http.Error(w, "account is blocked", http.StatusForbidden)
		return
	}

	token, _ := generateToken(username)
	setAuthCookie(w, token)
	log.Println("login (2fa):", username)
	recordLogin(r, username, loginOK, "password+totp")
	writeJSON(w, map[string]any{
		"token": token, "username": username,
		"must_change_password": mustChangePassword(username),
		"used_recovery_code":   usedRecovery,
		"recovery_left":        twoFARecoveryLeft(username),
	})
}

// twoFAIssuer — how the account is labelled in the authenticator app. The
// instance name rather than "Hexeris" in general: a person may have several,
// and there would otherwise be no way to tell them apart in the list.
func twoFAIssuer() string {
	if v := strings.TrimSpace(getEnvOrDefault("TOTP_ISSUER", "")); v != "" {
		return v
	}
	if cfg != nil && cfg.Domain != "" && cfg.Domain != "localhost" {
		return cfg.Domain
	}
	return "Hexeris"
}

// pngDataURI packs the image so it can be inlined straight into src. Fetching
// the QR with a separate request is not allowed: it shows the secret, and the
// address of such a request would settle in nginx's log and browser history.
func pngDataURI(png []byte) string {
	if len(png) == 0 {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}
