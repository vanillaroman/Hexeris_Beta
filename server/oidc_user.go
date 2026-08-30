package main

// Mapping an employee from the identity provider onto a Hexeris account.
//
// ═══ WHY THE BINDING IS BY sub, NOT BY EMAIL ══════════════════════════════
//
// This is the same lesson already paid for by Google sign-in (see auth.go): if
// the user is looked up by email or by name, then a match against SOMEONE
// ELSE'S local account means signing into it. A person with the address
// grace@partner.org would end up in the account "grace" that an administrator
// created for a different employee. The link is therefore held by the pair
// "issuer + subject" — at the provider that is immutable and never reused,
// unlike an email, which gets rewritten when someone's surname changes.
//
// ═══ WHY THE NAME IS NOT THE EMAIL ════════════════════════════════════════
//
// In Hexeris a user name is limited to [a-zA-Z0-9_]{2,32} (usernameRe), and
// that limit is not cosmetic: the name goes into the client's markup. A full
// email does not fit there and contains forbidden characters, so a name is
// derived from it by the same rules as for Google sign-in.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// initOIDCSchema adds the binding column.
//
// It is always called, even when provider sign-in is disabled: an empty column
// costs nothing, whereas flipping the switch would otherwise require a
// migration on a running server.
func initOIDCSchema() {
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS oidc_subject TEXT`); err != nil {
		log.Println("oidc schema:", err)
		return
	}
	// Uniqueness is a mandatory part of the protection: without it two rows
	// could refer to the same person at the provider, and which one they signed
	// into would be decided by the order of the query results.
	if _, err := db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc_subject ON users(oidc_subject) WHERE oidc_subject IS NOT NULL`); err != nil {
		log.Println("oidc schema index:", err)
	}
}

// oidcSubjectKey — a stable identifier for a person at the provider.
//
// issuer + subject rather than subject alone: a subject is unique only within
// its own provider, and when the provider changes (or there are two) a bare
// subject could collide between different people.
func oidcSubjectKey(claims jwt.MapClaims) string {
	iss, _ := claims["iss"].(string)
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return ""
	}
	return strings.TrimRight(iss, "/") + "#" + sub
}

// oidcUsernameFrom builds a Hexeris user name from an email per usernameRe.
func oidcUsernameFrom(email string) string {
	name := strings.Split(email, "@")[0]
	name = strings.ReplaceAll(name, ".", "_")
	name = nonUsernameCharRe.ReplaceAllString(name, "")
	if len(name) < 2 {
		// An empty or single-character name would not pass the schema. The
		// prefix makes it valid and shows where the record came from.
		name = "sso_" + name
	}
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

// oidcProvisionUser returns the Hexeris user name for the employee who signed
// in, creating the account if necessary.
//
// The order of the steps matters and repeats the already-proven Google path.
func oidcProvisionUser(claims jwt.MapClaims, cfg oidcConfig) (string, error) {
	subject := oidcSubjectKey(claims)
	if subject == "" {
		return "", errOIDC("the identity token has no subject — nothing to bind the account to")
	}
	email, err := oidcResolveUser(claims, cfg)
	if err != nil {
		return "", err
	}

	// 1. Has this person signed in before? The name comes from the database —
	//    the only reliable source: the name derived from the email may have
	//    been taken, in which case it was given a suffix at creation time.
	var bound string
	switch err := db.QueryRow(`SELECT username FROM users WHERE oidc_subject=$1`, subject).Scan(&bound); {
	case err == nil:
		return bound, nil
	case err != sql.ErrNoRows:
		return "", errOIDC("server error")
	}

	// 2. First sign-in. Creating a new employee is permitted only if
	//    registration is open OR their domain is on the allowlist — here the
	//    domain list is what acts as the invitation.
	if !registrationEnabled() && len(allowedEmailDomains()) == 0 {
		return "", errOIDC("this account does not exist in Hexeris yet, and self-registration is closed — " +
			"ask an administrator to create it, or set ALLOWED_EMAIL_DOMAINS")
	}

	// The password is random and told to nobody: signing in with it is not
	// intended, but the schema requires a non-empty hash. A deterministic
	// password (as in the old Google code) would mean that knowing the subject
	// grants a password sign-in.
	secret, rerr := randomURLSafe(32)
	if rerr != nil {
		return "", errOIDC("server error")
	}
	hash, herr := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if herr != nil {
		return "", errOIDC("server error")
	}

	base := oidcUsernameFrom(email)
	// Room for the suffix: the name must not exceed the schema's 32 characters.
	trimmed := base
	if len(trimmed) > 24 {
		trimmed = trimmed[:24]
	}
	// The suffix is derived from the subject rather than random: a repeat
	// attempt by the same person yields the same name, so a race between two
	// tabs does not multiply records.
	tail := strings.NewReplacer("-", "", "_", "").Replace(oidcHash(subject))
	for _, cand := range []string{base, trimmed + "_" + tail[:4], trimmed + "_" + tail[:8]} {
		if !usernameRe.MatchString(cand) {
			continue
		}
		// ON CONFLICT DO NOTHING returns no error, so the verdict comes from the
		// number of affected rows rather than from err.
		res, ierr := db.Exec(
			`INSERT INTO users(username, password_hash, oidc_subject) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`,
			cand, string(hash), subject)
		if ierr != nil {
			return "", errOIDC("server error")
		}
		if n, _ := res.RowsAffected(); n == 1 {
			log.Printf("oidc: created employee %q", cand)
			return cand, nil
		}
		// Nothing was inserted — either the name is taken, or this subject was
		// created by a concurrent request. The second case is checked at once:
		// otherwise one person's two tabs would give them two names.
		var again string
		if db.QueryRow(`SELECT username FROM users WHERE oidc_subject=$1`, subject).Scan(&again) == nil {
			return again, nil
		}
	}
	return "", errOIDC("could not pick a free user name for this account")
}

// oidcHash — a short stable tail derived from a string, for the name suffix.
// A hash rather than the subject itself: a subject can be long, contains
// characters that do not belong and is not meant to be displayed.
func oidcHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// errOIDC — an error whose text can be shown to a person. The messages here
// are written so that it is clear what to do, without revealing internals.
type oidcError string

func (e oidcError) Error() string { return string(e) }

func errOIDC(s string) error { return oidcError(s) }
