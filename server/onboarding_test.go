package main

// Admin-created accounts and the employee's own password change (requires
// TEST_DATABASE_URL).
//
// The whole path is covered rather than individual endpoints, because these
// tests answer the questions a customer's security team will ask:
//   1. Does the account stay "the admin's" while the admin knows the password?
//   2. Can a stolen token take the account over without knowing the password?
//   3. Does a password change sign the other devices out?
//   4. What happens to directory accounts, whose password lives elsewhere?

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// adminCreateUser calls the endpoint the way the panel does.
func adminCreateUser(t *testing.T, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv("ADMIN_KEY", "k")
	if cfg == nil {
		cfg = loadConfig()
	}
	body, _ := json.Marshal(map[string]string{
		"username": username, "action": "create", "password": password,
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/user-action", strings.NewReader(string(body)))
	req.Header.Set("X-Admin-Key", "k")
	req.RemoteAddr = "127.0.0.1:1"
	adminUserActionHandler(rr, req)
	return rr
}

// changePassword calls the change endpoint as the token's owner.
func changePassword(t *testing.T, token, oldPw, newPw string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"old_password": oldPw, "new_password": newPw})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/change-password", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:1"
	changePasswordHandler(rr, req)
	return rr
}

// The full path: an admin creates an employee, the employee signs in with
// the temporary password, changes it and becomes the account's owner.
func TestIntegrationAdminCreatesUserFlow(t *testing.T) {
	setupIntegration(t)
	user := uniqueName("it_inv")

	// With no password supplied the server generates one and returns it once.
	rr := adminCreateUser(t, user, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("create: status %d: %s", rr.Code, rr.Body.String())
	}
	var created struct {
		TempPassword string `json:"temp_password"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal("json:", err)
	}
	if len(created.TempPassword) < minPasswordLen {
		t.Fatalf("generated password is below the minimum length: %q", created.TempPassword)
	}
	// The alphabet excludes ambiguous characters, or the password cannot be
	// read aloud reliably.
	for _, c := range created.TempPassword {
		if !strings.ContainsRune(tempPasswordAlphabet, c) {
			t.Fatalf("character %q is outside the safe alphabet", c)
		}
	}

	// While the admin knows the password the account must demand a change.
	if !mustChangePassword(user) {
		t.Fatal("a new account must require a password change")
	}

	// The temporary password really works for signing in.
	token := loginAs(t, user, created.TempPassword)

	// A stolen token must not change the password without the current one,
	// or session theft becomes permanent account takeover.
	if rr := changePassword(t, token, "wrong-password", "brand-new-pass"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("change without the current password: want 401, got %d", rr.Code)
	}
	if !mustChangePassword(user) {
		t.Fatal("a failed attempt must not clear the must-change flag")
	}

	// A short password is rejected.
	if rr := changePassword(t, token, created.TempPassword, "short"); rr.Code != http.StatusBadRequest {
		t.Fatalf("short password: want 400, got %d", rr.Code)
	}

	// A successful change clears the flag and issues a fresh token.
	rr = changePassword(t, token, created.TempPassword, "n0v-parol-sotrudnika")
	if rr.Code != http.StatusOK {
		t.Fatalf("password change: status %d: %s", rr.Code, rr.Body.String())
	}
	var changed struct {
		Token string `json:"token"`
	}
	json.Unmarshal(rr.Body.Bytes(), &changed)
	if changed.Token == "" {
		t.Fatal("no new token after the change — the current session would drop")
	}
	if mustChangePassword(user) {
		t.Fatal("the flag must clear after the change")
	}

	// The old token is revoked, which is the point of changing a password.
	if _, ok := validateToken(token); ok {
		t.Fatal("the old token still works: other devices were not signed out")
	}
	// The new one works, or changing a password would eject the user.
	if who, ok := validateToken(changed.Token); !ok || who != user {
		t.Fatal("the new token was rejected")
	}

	// The old password no longer works and the new one does.
	if okLogin(t, user, created.TempPassword) {
		t.Fatal("the previous password still signs in")
	}
	if !okLogin(t, user, "n0v-parol-sotrudnika") {
		t.Fatal("the new password does not sign in")
	}
}

// The boundaries of the create endpoint.
func TestIntegrationAdminCreateValidation(t *testing.T) {
	setupIntegration(t)
	user := uniqueName("it_val")

	if rr := adminCreateUser(t, user, ""); rr.Code != http.StatusOK {
		t.Fatalf("first creation: %d", rr.Code)
	}
	// A repeated name is a conflict, not a silent overwrite.
	if rr := adminCreateUser(t, user, ""); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate: want 409, got %d", rr.Code)
	}
	// A name outside the character allowlist (a stored-XSS vector).
	if rr := adminCreateUser(t, "bad name<script>", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid name: want 400, got %d", rr.Code)
	}
	// A manually supplied short password is rejected.
	if rr := adminCreateUser(t, uniqueName("it_short"), "1234"); rr.Code != http.StatusBadRequest {
		t.Fatalf("short password: want 400, got %d", rr.Code)
	}
}

// A directory account's password lives in the directory and has no local
// counterpart; silently "changing" it here would create a local password
// that bypasses the directory.
func TestIntegrationLDAPUserCannotChangePassword(t *testing.T) {
	setupIntegration(t)
	user := uniqueName("it_ldap")
	// The row is created directly with the same marker ensureLDAPUser
	// writes: in the default build that function is a stub, so calling it
	// would make the test depend on the build tag.
	if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES($1,$2)`,
		user, ldapPasswordSentinel); err != nil {
		t.Fatal("seed ldap user:", err)
	}
	token, err := generateToken(user)
	if err != nil {
		t.Fatal(err)
	}
	rr := changePassword(t, token, "anything", "new-password-123")
	if rr.Code != http.StatusConflict {
		t.Fatalf("directory account: want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	// The stored hash must remain the marker, not become a bcrypt hash.
	var hash string
	db.QueryRow(`SELECT password_hash FROM users WHERE username=$1`, user).Scan(&hash)
	if hash != ldapPasswordSentinel {
		t.Fatalf("a local password replaced the directory marker: %q", hash)
	}
}

// Sign-in helpers.

func loginAs(t *testing.T, user, pw string) string {
	t.Helper()
	rr := doLogin(t, user, pw)
	if rr.Code != http.StatusOK {
		t.Fatalf("sign-in %s: status %d: %s", user, rr.Code, rr.Body.String())
	}
	var out struct {
		Token              string `json:"token"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal("json:", err)
	}
	if !out.MustChangePassword {
		t.Fatal("sign-in must tell the client a password change is required")
	}
	return out.Token
}

func okLogin(t *testing.T, user, pw string) bool {
	t.Helper()
	return doLogin(t, user, pw).Code == http.StatusOK
}

func doLogin(t *testing.T, user, pw string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	// A distinct address per call: /login counts failures per IP, and the
	// deliberately wrong attempts here would otherwise block later ones.
	req.RemoteAddr = uniqueName("10.0.") + ":1"
	loginHandler(rr, req)
	return rr
}
