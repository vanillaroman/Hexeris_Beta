package main

// Authentication: JWT, session cookies, register/login/Google, presence.

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// A dedicated client with a timeout for Google ID-token verification;
// http.DefaultClient has no timeout at all.
var googleTokenInfoClient = &http.Client{Timeout: 5 * time.Second}

// ─── JWT ─────────────────────────────────────────────────────────────────────

func generateToken(username string) (string, error) {
	return generateTokenAt(username, time.Now().Unix())
}

// generateTokenAt issues a token with an explicit iat.
//
// It is needed where old tokens are revoked and a new one issued in the same
// step: validateToken rejects `iat <= cutoff`, so a token minted in the same
// second as the revocation fails its own check and the user who just changed
// their password is signed straight out. One second of separation fixes that
// deterministically, without a race and without widening the window in which
// old tokens remain valid.
func generateTokenAt(username string, iat int64) (string, error) {
	claims := jwt.MapClaims{
		"sub": username,
		"exp": time.Unix(iat, 0).Add(30 * 24 * time.Hour).Unix(),
		"iat": iat,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(jwtSecret()))
}

func validateToken(tokenStr string) (string, bool) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(jwtSecret()), nil
	})
	if err != nil || !token.Valid {
		return "", false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", false
	}
	username, ok := claims["sub"].(string)
	if !ok {
		return "", false
	}
	iat, _ := claims["iat"].(float64)
	// Forced sign-out: an admin records the moment, and every token issued
	// earlier stops working. The check reads an in-memory cache rather than
	// the database on every request.
	if cutoff, exists := logoutCutoff(username); exists {
		if int64(iat) <= cutoff {
			return "", false
		}
	}
	return username, true
}

func extractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

// An HttpOnly cookie mirrors the JWT so <img> and <video> requests to
// /files/ authenticate without a token in the URL. SameSite=Strict covers
// CSRF, and the cookie is honoured only by filesHandler and
// /api/session-cookie; every other endpoint still takes a bearer token.

const authCookieName = "hexeris_auth"

// isSecureContext reports TLS mode. Marking the cookie Secure over plain
// HTTP would stop the browser sending it at all.
func isSecureContext() bool {
	return os.Getenv("TLS_MODE") != "http"
}

func setAuthCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   30 * 24 * 60 * 60, // = TTL JWT
		HttpOnly: true,
		Secure:   isSecureContext(),
		SameSite: http.SameSiteStrictMode,
	})
}

func clearAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecureContext(),
		SameSite: http.SameSiteStrictMode,
	})
}

// sessionCookieHandler sets the cookie from a valid bearer token on POST and
// clears it on sign-out via DELETE.
func sessionCookieHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		token := extractToken(r)
		if _, ok := validateToken(token); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		setAuthCookie(w, token)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		clearAuthCookie(w)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Status / key broadcast ───────────────────────────────────────────────────

func peerList(username string) []string {
	// Direct peers plus fellow members of shared groups. Without the second
	// part, presence never reaches people with whom there is only a group
	// in common, so a member who is online shows as offline there.
	//
	// Two separate branches rather than one OR with a CASE: the combined
	// form makes Postgres visit the heap for every row of the user just to
	// read one name — 679 blocks and 148 ms on a cold cache over 4000
	// messages, on every WS connect. During a mass reconnect that is 200
	// cold reads back to back and the pool stalls.
	//
	// Split branches fit covering indexes and read index-only:
	//   sender=$1    -> idx_messages_pair_seq(sender, recipient, seq)
	//   recipient=$1 -> idx_messages_recipient_seq_sender(recipient, seq) INCLUDE (sender)
	// On the same data: 15 blocks and 1.1 ms, 130× cheaper.
	rows, err := db.Query(`
		SELECT DISTINCT recipient AS peer FROM messages WHERE sender=$1
		UNION
		SELECT DISTINCT sender AS peer FROM messages WHERE recipient=$1
		UNION
		SELECT DISTINCT b.username
		FROM group_members a JOIN group_members b ON a.group_id = b.group_id
		WHERE a.username=$1 AND b.username <> $1`, username)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var peers []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			peers = append(peers, p)
		}
	}
	return peers
}

func broadcastStatus(username, status string) {
	broadcastStatusTo(username, status, peerList(username))
}

// broadcastStatusTo takes an already-computed peer list. A WS connect needs
// both directions (my status to them, theirs to me), and peerList is a heavy
// UNION over messages: computing it twice per connect doubles the database
// load exactly during a mass reconnect.
func broadcastStatusTo(username, status string, peers []string) {
	statusMsg := Message{Type: "status", From: username, Body: status}
	data, _ := json.Marshal(statusMsg)
	mu.RLock()
	defer mu.RUnlock()
	for _, peer := range peers {
		for _, c := range clients[peer] {
			c.send(data)
		}
	}
}

func sendOnlineStatuses(client *Client, peers []string) {
	mu.RLock()
	defer mu.RUnlock()
	for _, peer := range peers {
		if len(clients[peer]) > 0 {
			statusMsg := Message{Type: "status", From: peer, Body: "online"}
			data, _ := json.Marshal(statusMsg)
			client.send(data)
		}
	}
}

// ─── Auth handlers ─────────────────────────────────────────────────────────────

// usernameRe whitelists the only characters a username may contain. This is
// a security control, not just cosmetics: the web client renders usernames
// into the DOM (contact list, chat header) and historically did so without
// escaping, so an unrestricted username was a stored-XSS vector. Keep this
// strict even if the client-side escaping is fixed — defense in depth.
var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_]{2,32}$`)
var nonUsernameCharRe = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// Reserved usernames cannot be registered: an "admin" or "system" account
// created by an outsider misleads employees on any deployment.
var reservedUsernames = map[string]bool{
	"hexeris": true, "guide": true, "admin": true,
	"support": true, "system": true, "root": true,
}

func isReservedUsername(name string) bool {
	return reservedUsernames[strings.ToLower(strings.TrimSpace(name))]
}

// minPasswordLen applies to admin-issued and user-chosen passwords. Public
// self-registration keeps a shorter minimum on purpose: it is enabled on
// internal stands where accounts are disposable.
const minPasswordLen = 8

// The alphabet omits 0/O, 1/l/I and other pairs that are indistinguishable
// by voice or in a typeface: a temporary password is read aloud or copied
// off a screen, and that ambiguity is the usual reason a new employee cannot
// sign in on the first try.
const tempPasswordAlphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generateTempPassword draws 14 characters from crypto/rand (~83 bits).
// Rejection sampling rather than a plain modulo, which would favour the
// first characters of the alphabet and yield less entropy than claimed.
func generateTempPassword() (string, error) {
	const n = 14
	alpha := byte(len(tempPasswordAlphabet))
	limit := byte(256 - (256 % int(alpha)))
	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if buf[0] >= limit {
			continue
		}
		out = append(out, tempPasswordAlphabet[buf[0]%alpha])
	}
	return string(out), nil
}

// The password_hash of directory-provisioned accounts (see ensureLDAPUser).
// It matches no password, so local sign-in is impossible, and it is also the
// marker that rejects password changes here: those belong in the directory.
const ldapPasswordSentinel = "!ldap"

// mustChangePassword means an admin created the account and knows its
// password, so it does not yet belong to the employee.
func mustChangePassword(username string) bool {
	var v bool
	if db.QueryRow(`SELECT must_change_password FROM users WHERE username=$1`, username).Scan(&v) != nil {
		return false
	}
	return v
}

// POST /change-password  {old_password, new_password}
//
// Without this endpoint an employee whose password leaked cannot close the
// access themselves and has to ask an administrator for a reset.
func changePasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	me, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The same limiter as sign-in: this endpoint verifies the old password,
	// so it is another guessing point, merely behind a token.
	if loginUserLimiter.isBlocked(me) {
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.NewPassword) < minPasswordLen {
		http.Error(w, fmt.Sprintf("new password too short (min %d)", minPasswordLen), http.StatusBadRequest)
		return
	}
	if req.NewPassword == req.OldPassword {
		http.Error(w, "new password must differ from the current one", http.StatusBadRequest)
		return
	}

	var hash string
	switch err := db.QueryRow(`SELECT password_hash FROM users WHERE username=$1`, me).Scan(&hash); {
	case err == sql.ErrNoRows:
		// The token is still valid but the account is gone: a stale
		// session, not a server fault.
		http.Error(w, "account no longer exists", http.StatusUnauthorized)
		return
	case err != nil:
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if hash == ldapPasswordSentinel {
		http.Error(w, "this account is managed by the directory (LDAP/AD) — change the password there", http.StatusConflict)
		return
	}
	// The old password is always required, including on the first sign-in
	// with a temporary one: otherwise a stolen token takes the account
	// permanently by setting a new password without knowing the current.
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)) != nil {
		loginUserLimiter.recordFailure(me)
		log.Printf("change-password FAILED: user=%q ip=%s (wrong current password)", me, getIP(r))
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	// Changing a password must sign the other devices out — that is the
	// point of doing it after a suspected leak — while the current session
	// continues on a freshly issued token.
	now := time.Now().Unix()
	if _, err := db.Exec(
		`UPDATE users SET password_hash=$1, must_change_password=FALSE, forced_logout_at=$3 WHERE username=$2`,
		string(newHash), me, now); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	setLogoutCutoff(me, now)
	disconnectUser(me)

	// iat must be strictly after the revocation or the new token falls
	// under its own cutoff.
	token, _ := generateTokenAt(me, now+1)
	setAuthCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": token})
	log.Println("password changed:", me)
}

// googleClientID must match the OAuth Client ID configured in index.html's
// `data-client_id` (Google Identity Services). Required so /google-auth can
// verify the token's `aud` claim — see googleAuthHandler.
var googleClientID = os.Getenv("GOOGLE_CLIENT_ID")

// allowedReactionEmoji must stay in sync with the emoji-picker buttons in
// index.html (#emoji-picker). Anything not in this set is rejected before
// being routed to other clients.
var allowedReactionEmoji = map[string]bool{
	"👍": true, "❤️": true, "😂": true, "😮": true,
	"😢": true, "🔥": true, "👏": true, "🎉": true,
}

// Public self-registration, disabled by default: a corporate deployment on
// a public domain must not let strangers create accounts, or anyone who
// finds the URL is inside the company chat.
func registrationEnabled() bool {
	return os.Getenv("REGISTRATION_ENABLED") == "true"
}

// The domain allowlist for Google sign-in and Google-created accounts —
// a domain-wide invitation. Empty means no restriction.
func allowedEmailDomains() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("ALLOWED_EMAIL_DOMAINS"))
	if raw == "" {
		return nil
	}
	m := map[string]bool{}
	for _, d := range strings.Split(raw, ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			m[d] = true
		}
	}
	return m
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Public registration is off by default (see registrationEnabled).
	if !registrationEnabled() {
		http.Error(w, "public registration is disabled", http.StatusForbidden)
		return
	}
	ip := getIP(r)
	if registerLimiter.isBlocked(ip) {
		http.Error(w, "too many registration attempts, try again later", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernameRe.MatchString(req.Username) {
		http.Error(w, "username must be 2-32 characters: letters, digits, underscore only", http.StatusBadRequest)
		return
	}
	if isReservedUsername(req.Username) {
		http.Error(w, "this username is reserved", http.StatusConflict)
		return
	}
	// Second tier, by username: blocks enumeration across changing IPs.
	if registerUserLimiter.isBlocked(req.Username) {
		http.Error(w, "too many registration attempts for this username", http.StatusTooManyRequests)
		return
	}
	if len(req.Password) < 6 {
		http.Error(w, "password must be at least 6 characters", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	_, err = db.Exec("INSERT INTO users(username, password_hash) VALUES($1, $2)", req.Username, string(hash))
	if err != nil {
		registerLimiter.recordFailure(ip)
		registerUserLimiter.recordFailure(req.Username)
		http.Error(w, "username already taken", http.StatusConflict)
		return
	}
	token, _ := generateToken(req.Username)
	// A successful registration counts against the IP limit too, or one
	// address can create unlimited accounts.
	registerLimiter.recordFailure(ip)
	setAuthCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token, "username": req.Username})
	log.Println("registered:", req.Username)
}

// ─── /google-auth ─────────────────────────────────────────────────────────────

func googleAuthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Credential == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Rate-limited by IP: this endpoint is unauthenticated, calls out to
	// Google on every request and computes bcrypt when creating an account,
	// so without a limit it is a free load amplifier.
	ip := getIP(r)
	if googleAuthLimiter.isBlocked(ip) {
		http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
		return
	}
	googleAuthLimiter.recordFailure(ip)

	// Verify the Google ID token through the tokeninfo endpoint, using a
	// client with a timeout: an unbounded request would let a stalled
	// response hold a handler goroutine forever on an unauthenticated
	// endpoint. QueryEscape is required, or a credential containing '&'
	// rewrites the URL's parameters.
	resp, err := googleTokenInfoClient.Get(
		"https://oauth2.googleapis.com/tokeninfo?id_token=" + url.QueryEscape(req.Credential))
	if err != nil || resp.StatusCode != 200 {
		http.Error(w, "invalid google token", http.StatusUnauthorized)
		return
	}
	defer resp.Body.Close()

	var info struct {
		Email         string `json:"email"`
		EmailVerified string `json:"email_verified"`
		Sub           string `json:"sub"` // Google user ID
		Aud           string `json:"aud"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Email == "" {
		http.Error(w, "invalid token payload", http.StatusUnauthorized)
		return
	}
	if info.EmailVerified != "true" {
		http.Error(w, "email not verified", http.StatusUnauthorized)
		return
	}
	// Audience check — without this, ANY valid Google ID token issued to ANY
	// app (not just Hexeris) would be accepted here, letting an attacker
	// who obtained a token meant for a different site log in as that user.
	if googleClientID == "" {
		log.Println("google-auth rejected: GOOGLE_CLIENT_ID not configured on server")
		http.Error(w, "google auth not configured", http.StatusInternalServerError)
		return
	}
	if info.Aud != googleClientID {
		http.Error(w, "invalid token audience", http.StatusUnauthorized)
		return
	}

	// When ALLOWED_EMAIL_DOMAINS is set, only those domains may sign in or
	// be created. Without it, Google sign-in is a way around disabled
	// registration for any Google account at all.
	allowedDomains := allowedEmailDomains()
	emailDomain := ""
	if at := strings.LastIndex(info.Email, "@"); at >= 0 {
		emailDomain = strings.ToLower(info.Email[at+1:])
	}
	if allowedDomains != nil && !allowedDomains[emailDomain] {
		http.Error(w, "email domain not allowed", http.StatusForbidden)
		return
	}

	// The email local part becomes the username, normalised to the same
	// character allowlist as ordinary registration: it may contain +, - or
	// ., which would otherwise reach the client's DOM unescaped.
	username := strings.Split(info.Email, "@")[0]
	username = strings.ReplaceAll(username, ".", "_")
	username = nonUsernameCharRe.ReplaceAllString(username, "")
	if len(username) < 2 {
		username = "g_" + username
	}
	if len(username) > 32 {
		username = username[:32]
	}

	// If this Google account is already bound, the username comes from the
	// database, the only reliable source: the email prefix may be taken.
	var boundUser string
	err = db.QueryRow("SELECT username FROM users WHERE google_sub=$1", info.Sub).Scan(&boundUser)
	switch {
	case err == nil:
		username = boundUser
	case err != sql.ErrNoRows:
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	default:
		// Not bound. If the username is taken, check whether it is a legacy
		// Google account, whose password was derived deterministically; a
		// match binds it. Otherwise it is someone else's local account and
		// must not be entered — signing in there is account takeover by a
		// matching email prefix — so a suffixed account is created.
		var hash string
		if db.QueryRow("SELECT password_hash FROM users WHERE username=$1 AND google_sub IS NULL", username).Scan(&hash) == nil &&
			bcrypt.CompareHashAndPassword([]byte(hash), []byte(info.Sub+info.Email)) == nil {
			db.Exec("UPDATE users SET google_sub=$1 WHERE username=$2", info.Sub, username)
			log.Println("google legacy account bound:", username)
			break
		}
		// Creating a new account through Google requires registration to be
		// enabled or the domain to be allowlisted; signing existing Google
		// accounts in is not restricted by this.
		if !registrationEnabled() && allowedDomains == nil {
			http.Error(w, "registration is disabled", http.StatusForbidden)
			return
		}
		// Candidates carry a growing suffix derived from sub. Success is
		// judged by RowsAffected, since ON CONFLICT DO NOTHING returns no
		// error and an err check would leave this branch dead.
		newHash, _ := bcrypt.GenerateFromPassword([]byte(info.Sub+info.Email), bcrypt.DefaultCost)
		base := username
		if len(base) > 23 {
			base = base[:23] // room for "_" plus an 8-character suffix
		}
		created := false
		for _, cand := range []string{base, base + "_" + info.Sub[:4], base + "_" + info.Sub[:8]} {
			res, ierr := db.Exec("INSERT INTO users(username, password_hash, google_sub) VALUES($1,$2,$3) ON CONFLICT DO NOTHING",
				cand, string(newHash), info.Sub)
			if n, _ := res.RowsAffected(); ierr == nil && n == 1 {
				username = cand
				created = true
				log.Println("google registered:", username, info.Email)
				break
			}
		}
		if !created {
			http.Error(w, "username unavailable", http.StatusConflict)
			return
		}
	}

	// Blocking is checked before a token is issued, as in /login. Without
	// it, Google sign-in bypasses a block: the blocked employee signs in and
	// receives a token whose iat is newer than the revocation cutoff. The
	// WS handler rejects them, but every HTTP endpoint verifies only the
	// signature, so a blocked account would keep reading conversations and
	// downloading files.
	if userBlocked(username) {
		log.Printf("google-auth REJECTED: user=%q ip=%s (account blocked)", username, ip)
		recordLogin(r, username, loginBlocked, "google")
		http.Error(w, "account is blocked", http.StatusForbidden)
		return
	}

	recordLogin(r, username, loginOK, "google")
	token, _ := generateToken(username)
	setAuthCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token, "username": username})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ip := getIP(r)
	if loginLimiter.isBlocked(ip) {
		// Log the block itself, or "sign-in does not work" has no
		// explanation on the operator's side. The body is not parsed yet,
		// so only the address is known here.
		log.Printf("login BLOCKED by rate limit: ip=%s", ip)
		recordLogin(r, "", loginRateLimit, "password")
		http.Error(w, "too many attempts, try again in 10 minutes", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	// Second tier, by username: the IP limiter cannot see a distributed
	// guess at one account's password.
	if loginUserLimiter.isBlocked(req.Username) {
		http.Error(w, "too many attempts for this account, try again later", http.StatusTooManyRequests)
		return
	}

	// Local password first, which covers admin and service accounts that do
	// not exist in the directory.
	var hash string
	localErr := db.QueryRow("SELECT password_hash FROM users WHERE username=$1", req.Username).Scan(&hash)
	localOK := localErr == nil && bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil

	// Then the directory, provisioning a local record on success without
	// storing a usable password. A directory outage is not treated as a
	// wrong password: it is logged and refused without a false reason.
	if !localOK && ldapEnabled() {
		ok, lerr := ldapAuthenticate(req.Username, req.Password)
		if lerr != nil {
			log.Println("ldap auth error for", req.Username, ":", lerr)
		}
		if ok {
			if !usernameRe.MatchString(req.Username) {
				http.Error(w, "directory username has unsupported characters (allowed: letters, digits, underscore, 2-32)", http.StatusBadRequest)
				return
			}
			if err := ensureLDAPUser(req.Username); err != nil {
				http.Error(w, "server error", http.StatusInternalServerError)
				return
			}
			localOK = true
		}
	}

	if !localOK {
		loginLimiter.recordFailure(ip)
		loginUserLimiter.recordFailure(req.Username)
		// Failed sign-ins must be logged, or the limiter blocks an address
		// on invisible events and nobody can explain why sign-in stopped
		// working for everyone. Behind a corporate NAT this matters most:
		// five typos from one colleague lock out the whole office. The
		// password itself is never logged.
		log.Printf("login FAILED: user=%q ip=%s (wrong password or no such account)",
			req.Username, ip)
		recordLogin(r, req.Username, loginBadCreds, "password")
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	// Checked after the password, or the difference in responses reveals
	// which accounts are blocked without knowing any password.
	if userBlocked(req.Username) {
		log.Printf("login REJECTED: user=%q ip=%s (account blocked)", req.Username, ip)
		recordLogin(r, req.Username, loginBlocked, "password")
		http.Error(w, "account is blocked", http.StatusForbidden)
		return
	}
	// The second factor comes BEFORE the token is issued. A token "while the check
	// runs" would mean having no second factor: it already reads the
	// correspondence. A ticket is issued instead, granting nothing but the right
	// to present a code (see twofa.go).
	if twoFAEnabled(req.Username) {
		ticket, terr := twoFAIssueTicket(req.Username)
		if terr != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		log.Printf("login: %s — password accepted, awaiting the second-factor code", req.Username)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"twofa_required": true,
			"ticket":         ticket,
			"username":       req.Username,
		})
		return
	}

	token, _ := generateToken(req.Username)
	setAuthCookie(w, token)
	w.Header().Set("Content-Type", "application/json")
	// must_change_password tells the client to show the password screen
	// before the chat: while someone other than the owner knows the
	// password, the account is not yet theirs.
	json.NewEncoder(w).Encode(map[string]any{
		"token": token, "username": req.Username,
		"must_change_password": mustChangePassword(req.Username),
	})
	log.Println("login:", req.Username)
	recordLogin(r, req.Username, loginOK, "password")
}
