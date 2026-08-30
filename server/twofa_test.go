package main

// The second factor end to end: enabling, signing in, recovery and reset
// (requires TEST_DATABASE_URL).
//
// What is checked first is the reason the second factor exists at all: one
// stolen password is not enough. And then what quietly cancels the protection:
// reusing a code, unlimited guessing on a single ticket, turning it off from
// an open session, the secret stored in plaintext.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var twoFASchemaOnce sync.Once

func setupTwoFA(t *testing.T) {
	t.Helper()
	setupIntegration(t)
	twoFASchemaOnce.Do(func() {
		initAdminSchema()
		initTwoFASchema()
	})
}

// makeUser creates an employee with a known password.
func makeUser(t *testing.T, password string) string {
	t.Helper()
	name := uniqueName("tf")
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES($1,$2)`,
		name, string(hash)); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM twofa_recovery WHERE username=$1`, name)
		db.Exec(`DELETE FROM users WHERE username=$1`, name)
	})
	return name
}

// enable2FA turns the second factor on directly and returns the secret.
func enable2FA(t *testing.T, user string) string {
	t.Helper()
	secret, err := totpNewSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`UPDATE users SET totp_secret=$2, totp_enabled=TRUE, totp_last_step=0 WHERE username=$1`,
		user, encryptBody(secret)); err != nil {
		t.Fatalf("enabling 2fa: %v", err)
	}
	return secret
}

func codeNow(t *testing.T, secret string) string {
	t.Helper()
	code, err := totpCodeAt(secret, totpStep(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// post calls a handler with a JSON body and its own address (the sign-in
// limiter counts by IP — a shared address would couple the tests).
func post(t *testing.T, h http.HandlerFunc, path, token string, body any, ip string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.RemoteAddr = ip + ":1234"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("the response is not JSON (%d): %s", rr.Code, rr.Body.String())
	}
	return out
}

var testIP = func() func() string {
	var n int
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("198.51.%d.%d", n/250+1, n%250+1)
	}
}()

// ── Signing in ────────────────────────────────────────────────────────────

// THE MAIN POINT: the right password is not enough. Everything else is
// written for the sake of this one line.
func TestLoginWithTwoFAGivesTicketNotToken(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "correct horse battery")
	enable2FA(t, user)

	rr := post(t, loginHandler, "/login", "",
		map[string]string{"username": user, "password": "correct horse battery"}, testIP())
	if rr.Code != http.StatusOK {
		t.Fatalf("sign-in with the correct password returned %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeJSON(t, rr)

	if got["twofa_required"] != true {
		t.Fatal("the server did not demand a second factor — a stolen password suffices")
	}
	if got["token"] != nil {
		t.Fatal("a token was issued BEFORE the second factor — it already reads messages")
	}
	if s, _ := got["ticket"].(string); s == "" {
		t.Fatal("no ticket was issued — there is nothing to present at the second step")
	}
	// No cookie may be set either: it is equivalent to a token.
	for _, c := range rr.Result().Cookies() {
		if strings.Contains(strings.ToLower(c.Name), "auth") || strings.Contains(strings.ToLower(c.Name), "token") {
			t.Fatalf("cookie %q was set before the second factor", c.Name)
		}
	}
}

func TestTwoFAVerifyIssuesToken(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-verify-ok")
	secret := enable2FA(t, user)

	ticket, err := twoFAIssueTicket(user)
	if err != nil {
		t.Fatal(err)
	}
	rr := post(t, twoFAVerifyHandler, "/auth/2fa/verify", "",
		map[string]string{"ticket": ticket, "code": codeNow(t, secret)}, testIP())
	if rr.Code != http.StatusOK {
		t.Fatalf("a correct code was rejected: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeJSON(t, rr)
	tok, _ := got["token"].(string)
	if tok == "" {
		t.Fatal("no token was issued after a correct code")
	}
	if who, ok := validateToken(tok); !ok || who != user {
		t.Fatalf("the issued token does not validate: who=%q ok=%v", who, ok)
	}
}

// A code is single-use. Without that, a code read over your shoulder keeps
// working for another minute and a half — the length of the tolerance window.
func TestTwoFACodeCannotBeReused(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-replay")
	secret := enable2FA(t, user)
	code := codeNow(t, secret)

	if !twoFAConsume(user, code) {
		t.Fatal("the code must work the first time")
	}
	if twoFAConsume(user, code) {
		t.Fatal("the same code was accepted twice — a glimpsed code keeps working")
	}
	// The earlier window is closed too: otherwise the reuse check is bypassed
	// with a code captured a second earlier.
	older, _ := totpCodeAt(secret, totpStep(time.Now())-1)
	if twoFAConsume(user, older) {
		t.Fatal("a code from an earlier window was accepted")
	}
}

// A ticket is not an unlimited pass for guessing.
func TestTwoFATicketBurnsOutAfterFailures(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-burn")
	enable2FA(t, user)
	ticket, _ := twoFAIssueTicket(user)

	for i := 0; i < twoFATicketAttempts; i++ {
		if _, ok := twoFAClaimAttempt(ticket); !ok {
			t.Fatalf("the ticket died on attempt %d of %d", i+1, twoFATicketAttempts)
		}
	}
	if _, ok := twoFAClaimAttempt(ticket); ok {
		t.Fatalf("the ticket survived %d attempts — codes can be guessed with it", twoFATicketAttempts)
	}
	// Someone else's ticket, and an invented one, never pass.
	if _, ok := twoFAClaimAttempt("made-up-ticket"); ok {
		t.Fatal("a non-existent ticket was accepted")
	}
}

func TestTwoFATicketExpires(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-exp")
	ticket, _ := twoFAIssueTicket(user)

	// The deadline is rewound by hand: waiting five minutes in a test is not an
	// option, and faking time everywhere costs more than ageing one row.
	twoFAMu.Lock()
	twoFATickets[ticket].expires = time.Now().Add(-time.Second)
	twoFAMu.Unlock()

	if _, ok := twoFAClaimAttempt(ticket); ok {
		t.Fatal("an expired ticket still works")
	}
}

// ── Turning it on and off ─────────────────────────────────────────────────

// The second factor must not switch on before a code confirms it: otherwise
// the first result is an employee locked out by a failed scan.
func TestTwoFAEnableRequiresWorkingCode(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-enable")
	token, _ := generateToken(user)

	rr := post(t, twoFASetupHandler, "/auth/2fa/setup", token, nil, testIP())
	if rr.Code != http.StatusOK {
		t.Fatalf("setup returned %d: %s", rr.Code, rr.Body.String())
	}
	setup := decodeJSON(t, rr)
	secret, _ := setup["secret"].(string)
	if secret == "" {
		t.Fatal("setup returned no secret")
	}
	if uri, _ := setup["uri"].(string); !strings.HasPrefix(uri, "otpauth://") {
		t.Errorf("setup returned no app URI: %q", uri)
	}
	if qr, _ := setup["qr_png"].(string); !strings.HasPrefix(qr, "data:image/png;base64,") {
		t.Errorf("setup returned no image: %.40q", qr)
	}
	// Until a code confirms it, the factor stays off.
	if twoFAEnabled(user) {
		t.Fatal("the second factor switched on before a code confirmed it")
	}

	if rr := post(t, twoFAEnableHandler, "/auth/2fa/enable", token,
		map[string]string{"code": "000000"}, testIP()); rr.Code == http.StatusOK {
		t.Fatal("enabling succeeded with a random code")
	}
	if twoFAEnabled(user) {
		t.Fatal("a failed confirmation enabled the factor anyway")
	}

	rr = post(t, twoFAEnableHandler, "/auth/2fa/enable", token,
		map[string]string{"code": codeNow(t, secret)}, testIP())
	if rr.Code != http.StatusOK {
		t.Fatalf("a correct code did not enable the factor: %d %s", rr.Code, rr.Body.String())
	}
	if !twoFAEnabled(user) {
		t.Fatal("the factor did not switch on")
	}
	codes, _ := decodeJSON(t, rr)["recovery_codes"].([]any)
	if len(codes) != twoFARecoveryCount {
		t.Fatalf("%d recovery codes were issued, expected %d", len(codes), twoFARecoveryCount)
	}

	// A second setup while the factor is on means swapping the secret from an
	// already open session, i.e. moving the second factor to someone else's phone.
	if rr := post(t, twoFASetupHandler, "/auth/2fa/setup", token, nil, testIP()); rr.Code != http.StatusConflict {
		t.Errorf("a second setup with the factor on returned %d, expected 409", rr.Code)
	}
}

// Turning it off cancels the protection, and one open session is not enough.
func TestTwoFADisableNeedsPasswordAndCode(t *testing.T) {
	setupTwoFA(t)
	const pw = "pw-disable-me"
	user := makeUser(t, pw)
	secret := enable2FA(t, user)
	token, _ := generateToken(user)

	// A code alone, without the password.
	if rr := post(t, twoFADisableHandler, "/auth/2fa/disable", token,
		map[string]string{"code": codeNow(t, secret)}, testIP()); rr.Code == http.StatusOK {
		t.Fatal("the factor was turned off without a password — an open laptop suffices")
	}
	// The password alone, without a code.
	if rr := post(t, twoFADisableHandler, "/auth/2fa/disable", token,
		map[string]string{"password": pw}, testIP()); rr.Code == http.StatusOK {
		t.Fatal("the factor was turned off without a code — a stolen password suffices")
	}
	if !twoFAEnabled(user) {
		t.Fatal("the failed attempts turned the factor off after all")
	}

	rr := post(t, twoFADisableHandler, "/auth/2fa/disable", token,
		map[string]string{"password": pw, "code": codeNow(t, secret)}, testIP())
	if rr.Code != http.StatusOK {
		t.Fatalf("turning off with a password and a code returned %d: %s", rr.Code, rr.Body.String())
	}
	if twoFAEnabled(user) {
		t.Fatal("the factor did not switch off")
	}
}

// ── Recovery codes ────────────────────────────────────────────────────────

func TestRecoveryCodesWorkOnceEach(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-recovery")
	enable2FA(t, user)

	codes, err := twoFANewRecoveryCodes(user)
	if err != nil {
		t.Fatal(err)
	}
	if n := twoFARecoveryLeft(user); n != twoFARecoveryCount {
		t.Fatalf("%d codes remain, expected %d", n, twoFARecoveryCount)
	}
	if !twoFAUseRecovery(user, codes[0]) {
		t.Fatal("a fresh recovery code did not work")
	}
	if twoFAUseRecovery(user, codes[0]) {
		t.Fatal("a recovery code worked twice")
	}
	if n := twoFARecoveryLeft(user); n != twoFARecoveryCount-1 {
		t.Errorf("%d codes remain, expected %d", n, twoFARecoveryCount-1)
	}
	// The other codes from the same list are still alive.
	if !twoFAUseRecovery(user, codes[1]) {
		t.Error("the remaining codes in the list stopped working")
	}
	// An invented code does not pass.
	if twoFAUseRecovery(user, "AAAA-BBBB-CCCC-DDDD") {
		t.Error("an invented recovery code was accepted")
	}

	// Reissuing invalidates the previously printed list.
	if _, err := twoFANewRecoveryCodes(user); err != nil {
		t.Fatal(err)
	}
	if twoFAUseRecovery(user, codes[2]) {
		t.Error("a code from the old list worked after reissuing")
	}
}

// A recovery code replaces the one-time code at sign-in — otherwise losing a
// phone would always mean going to an administrator.
func TestRecoveryCodeSignsIn(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-rec-login")
	enable2FA(t, user)
	codes, _ := twoFANewRecoveryCodes(user)

	ticket, _ := twoFAIssueTicket(user)
	rr := post(t, twoFAVerifyHandler, "/auth/2fa/verify", "",
		map[string]string{"ticket": ticket, "code": codes[0]}, testIP())
	if rr.Code != http.StatusOK {
		t.Fatalf("signing in with a recovery code returned %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeJSON(t, rr)
	if got["used_recovery_code"] != true {
		t.Error("the response does not mark recovery-code use — nobody learns the list is shrinking")
	}
	if got["token"] == nil {
		t.Fatal("no token was issued")
	}
}

// ── Storage and reset ─────────────────────────────────────────────────────

// The TOTP secret IS the second factor: whoever reads it from the database
// generates codes themselves. In plaintext a dump would cancel the protection.
func TestTwoFASecretIsEncryptedAtRest(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-at-rest")
	secret := enable2FA(t, user)

	var stored string
	if err := db.QueryRow(`SELECT totp_secret FROM users WHERE username=$1`, user).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == secret {
		t.Fatal("the secret is stored in plaintext")
	}
	if strings.Contains(stored, secret[:8]) {
		t.Fatal("a fragment of the secret is visible in the database")
	}
	// A control: it does read back correctly, otherwise the test would pass on
	// "corrupted on write" as well.
	got, enabled, err := twoFASecret(user)
	if err != nil || !enabled || got != secret {
		t.Fatalf("the secret does not read back: got=%q enabled=%v err=%v", got, enabled, err)
	}
}

func TestAdminResetClearsEverything(t *testing.T) {
	setupTwoFA(t)
	user := makeUser(t, "pw-admin-reset")
	enable2FA(t, user)
	codes, _ := twoFANewRecoveryCodes(user)

	if err := twoFAReset(user); err != nil {
		t.Fatal(err)
	}
	if twoFAEnabled(user) {
		t.Fatal("the factor is still on after a reset")
	}
	if n := twoFARecoveryLeft(user); n != 0 {
		t.Errorf("%d recovery codes remain after a reset", n)
	}
	if twoFAUseRecovery(user, codes[0]) {
		t.Error("an old recovery code worked after a reset")
	}
	// And signing in is a single step again.
	rr := post(t, loginHandler, "/login", "",
		map[string]string{"username": user, "password": "pw-admin-reset"}, testIP())
	if rr.Code != http.StatusOK {
		t.Fatalf("signing in after a reset returned %d: %s", rr.Code, rr.Body.String())
	}
	if decodeJSON(t, rr)["token"] == nil {
		t.Fatal("sign-in still demands a second factor after a reset")
	}
}
