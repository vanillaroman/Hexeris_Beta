package main

// Time-based one-time codes (TOTP, RFC 6238) — the second factor for local
// password sign-in.
//
// ═══ WHY TOTP SPECIFICALLY ════════════════════════════════════════════════
//
// For employees who sign in through a corporate provider, the second factor is
// supplied by the provider (see docs/engineering/SSO-OIDC.md). Local passwords
// had none at all — and local accounts always exist: administrators, service
// accounts, contractors, and the whole pilot while no provider is connected
// yet.
//
// TOTP was chosen because it demands nothing of the customer's infrastructure:
// no SMS gateway (which costs money and is defeated by a SIM swap), no push
// service, no outbound access. An app on a phone and a shared secret, and that
// is all. It works with Google Authenticator, Aegis, 1Password and any other
// app that understands otpauth://.
//
// ═══ WHAT IS HERE AND WHAT IS IN twofa.go ═════════════════════════════════
//
// Only the algorithm is here: the secret, the code, the check, the app URI. No
// database, no HTTP — so all of it can be verified against the table of
// reference values from the RFC itself rather than through a live server.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// A 30-second step and 6 digits are what every app expects by default.
	// Deviating is technically possible, but apps show those values only on
	// manual entry, and half the customers would get "the code does not work"
	// out of nowhere.
	totpStepSeconds = 30
	totpDigits      = 6

	// A tolerance of one step each way — a window of a minute and a half.
	// Without it, a few seconds of drift between phone and server clocks makes
	// signing in impossible exactly when the code changes. More than one step
	// must not be taken: the window is precisely the time during which a
	// glimpsed code still works.
	totpSkew = 1

	// 20 bytes = 160 bits, as in RFC 6238. In base32 that is 32 characters —
	// exactly as many as a person is still willing to type by hand if the
	// camera does not work.
	totpSecretBytes = 20
)

// totpBase32 — without '=' padding. Apps accept it, but there is no reason to
// show a person a string with a tail of equals signs.
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpNewSecret — the shared secret for one account.
func totpNewSecret() (string, error) {
	b := make([]byte, totpSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return totpBase32.EncodeToString(b), nil
}

// totpStep — the number of the time window. A separate function because that
// same number is stored in the database as "the last one used": without it a
// code read over your shoulder works for another minute and a half.
func totpStep(t time.Time) int64 {
	return t.Unix() / totpStepSeconds
}

// totpCodeAt computes the code for one particular window.
func totpCodeAt(secret string, step int64) (string, error) {
	key, err := totpBase32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("bad secret: %w", err)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))

	// HMAC-SHA1 not because SHA-1 is good but because that is what RFC 4226
	// says and what every app does. It is used here as a PRF with a secret key
	// rather than as a hash over known data: the known collision attacks on
	// SHA-1 do not apply to this use.
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation from RFC 4226 §5.3.
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off]&0x7f) << 24) |
		(uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) |
		uint32(sum[off+3])

	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, v%mod), nil
}

// totpVerify checks a code and returns the number of the window that matched.
//
// The caller needs the number: it should be remembered so a code from the
// same (or an earlier) window is not accepted twice.
func totpVerify(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	// Internal spaces: apps display the code as "123 456" and people copy it
	// along with the space.
	code = strings.ReplaceAll(code, " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := totpStep(now)
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		want, err := totpCodeAt(secret, cur+d)
		if err != nil {
			return 0, false
		}
		// A constant-time comparison: a difference in response time reveals how
		// many leading digits were guessed and turns a search of a million
		// options into six searches of ten.
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return cur + d, true
		}
	}
	return 0, false
}

// totpURI — the URI the app reads from the screen.
//
// The otpauth:// format is described by the Google Authenticator scheme and is
// supported by all. The issuer is given twice (in the label and the parameter)
// deliberately: old apps read only the label, new ones only the parameter, and
// without the duplicate the entry is listed under the bare user name.
func totpURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpStepSeconds))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
