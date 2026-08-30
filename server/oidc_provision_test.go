package main

// Creating an employee on the first provider sign-in (requires
// TEST_DATABASE_URL). Two of the costliest failures are checked here: signing
// into SOMEONE ELSE'S account on a name match, and an account appearing where
// creating one is not permitted.

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

var oidcSchemaOnce sync.Once

func setupOIDCDB(t *testing.T) {
	t.Helper()
	setupIntegration(t)
	oidcSchemaOnce.Do(initOIDCSchema)
}

func oidcClaims(iss, sub, email string) jwt.MapClaims {
	return jwt.MapClaims{"iss": iss, "sub": sub, "email": email, "email_verified": true}
}

func dropUser(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() { db.Exec(`DELETE FROM users WHERE username=$1`, name) })
}

// THE MAIN POINT: a name match must NOT grant access to someone else's account.
// Google sign-in was burned by exactly this once, and there is no reason to
// repeat the lesson in new code.
func TestOIDCProvisionNeverHijacksExistingAccount(t *testing.T) {
	setupOIDCDB(t)
	os.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")
	defer os.Unsetenv("ALLOWED_EMAIL_DOMAINS")
	cfg := oidcConfig{UsernameClaim: "email"}

	victim := uniqueName("grace")
	if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES($1,'x')`, victim); err != nil {
		t.Fatalf("creating someone else's account: %v", err)
	}
	dropUser(t, victim)

	// An outsider whose email derives exactly the same name.
	got, err := oidcProvisionUser(oidcClaims("https://sso.example.com", "outsider-1", victim+"@example.com"), cfg)
	if err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	if got == victim {
		t.Fatal("signed into SOMEONE ELSE'S account on a name match")
	}
	if !strings.HasPrefix(got, victim[:min(len(victim), 24)]) {
		t.Errorf("the name does not look derived from the email: %q", got)
	}
	dropUser(t, got)

	// Someone else's account must not end up bound to the provider.
	var bound *string
	db.QueryRow(`SELECT oidc_subject FROM users WHERE username=$1`, victim).Scan(&bound)
	if bound != nil {
		t.Errorf("someone else's account is bound to the provider: %v", *bound)
	}
}

// A repeat sign-in must yield THE SAME account, not create a new one.
func TestOIDCProvisionIsStableAcrossLogins(t *testing.T) {
	setupOIDCDB(t)
	os.Setenv("ALLOWED_EMAIL_DOMAINS", "example.com")
	defer os.Unsetenv("ALLOWED_EMAIL_DOMAINS")
	cfg := oidcConfig{UsernameClaim: "email"}

	email := uniqueName("ada") + "@example.com"
	c := oidcClaims("https://sso.example.com", "ada-sub-1", email)

	first, err := oidcProvisionUser(c, cfg)
	if err != nil {
		t.Fatalf("the first sign-in: %v", err)
	}
	dropUser(t, first)

	second, err := oidcProvisionUser(c, cfg)
	if err != nil {
		t.Fatalf("the second sign-in: %v", err)
	}
	if second != first {
		t.Fatalf("the second sign-in gave a different name: %q against %q", second, first)
	}

	// An email change at the provider (a new surname, a domain rebrand) must not
	// breed a second account: the binding holds on to the subject.
	renamed := oidcClaims("https://sso.example.com", "ada-sub-1", uniqueName("ada_new")+"@example.com")
	third, err := oidcProvisionUser(renamed, cfg)
	if err != nil {
		t.Fatalf("sign-in after an email change: %v", err)
	}
	if third != first {
		t.Errorf("the email change created a second account: %q against %q", third, first)
	}

	// The same subject at a DIFFERENT provider is a different person.
	other := oidcClaims("https://other-idp.example.com", "ada-sub-1", uniqueName("zoe")+"@example.com")
	fourth, err := oidcProvisionUser(other, cfg)
	if err != nil {
		t.Fatalf("a different provider: %v", err)
	}
	if fourth == first {
		t.Error("a subject match across providers granted someone else's account")
	}
	dropUser(t, fourth)
}

// Without permission to create employees, SSO must create no accounts.
func TestOIDCProvisionRespectsRegistrationGate(t *testing.T) {
	setupOIDCDB(t)
	os.Unsetenv("ALLOWED_EMAIL_DOMAINS")
	old := os.Getenv("REGISTRATION_ENABLED")
	os.Unsetenv("REGISTRATION_ENABLED")
	defer os.Setenv("REGISTRATION_ENABLED", old)

	cfg := oidcConfig{UsernameClaim: "email"}
	name, err := oidcProvisionUser(
		oidcClaims("https://sso.example.com", "nobody-1", uniqueName("stranger")+"@anywhere.com"), cfg)
	if err == nil {
		dropUser(t, name)
		t.Fatal("an account was created with registration closed and an empty allowlist")
	}
	if !strings.Contains(err.Error(), "administrator") {
		t.Errorf("the reason does not suggest a way out: %q", err)
	}
}

// The name is derived by the same rules as ordinary registration: otherwise
// the schema rejects the insert and the name reaches the client's markup as is.
func TestOIDCUsernameFrom(t *testing.T) {
	cases := map[string]string{
		"grace.hopper@example.com": "grace_hopper",
		"ada+news@example.com":     "adanews",
		"a@example.com":            "sso_a",
		"@example.com":             "sso_",
		"Very.Long.Name.That.Goes.On.And.On@example.com": "Very_Long_Name_That_Goes_On_And_O",
	}
	for in, want := range cases {
		got := oidcUsernameFrom(in)
		if in == "@example.com" {
			// An empty local part — all that matters is a valid result.
			if !usernameRe.MatchString(got) {
				t.Errorf("%q produced the invalid name %q", in, got)
			}
			continue
		}
		if len(want) > 32 {
			want = want[:32]
		}
		if got != want {
			t.Errorf("%q: got %q, expected %q", in, got, want)
		}
		if !usernameRe.MatchString(got) {
			t.Errorf("%q produced a name that fails usernameRe: %q", in, got)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
