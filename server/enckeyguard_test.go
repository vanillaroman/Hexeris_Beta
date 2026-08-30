package main

// Protection against a substituted SERVER_ENC_KEY.
//
// Exactly one property is checked, the most expensive one: a server with a
// FOREIGN key must not count as healthy. A mistake in that direction is not
// visible at once — it shows up after a few days of operation, when the
// database already mixes two encryption eras and the old data is beyond rescue.

import (
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
)

var encKeySchemaOnce sync.Once

func setupEncKeyGuard(t *testing.T) {
	t.Helper()
	setupIntegration(t)
	encKeySchemaOnce.Do(initEncKeyGuardSchema)

	// There is one fingerprint row per database and the tests rewrite it. The
	// pre-test value is restored, otherwise a test run would leave behind a
	// database that a real server would refuse to start against.
	var before string
	err := db.QueryRow(`SELECT value FROM server_meta WHERE key=$1`, encKeyMetaKey).Scan(&before)
	had := err == nil
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("reading server_meta: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM server_meta WHERE key=$1`, encKeyMetaKey)
		if had {
			db.Exec(`INSERT INTO server_meta(key, value) VALUES($1,$2)`, encKeyMetaKey, before)
		}
	})
}

// clearEncKeyRow brings the database to the "the guard has never run" state.
func clearEncKeyRow(t *testing.T) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM server_meta WHERE key=$1`, encKeyMetaKey); err != nil {
		t.Fatalf("clearing server_meta: %v", err)
	}
}

func storedFingerprint(t *testing.T) string {
	t.Helper()
	var v string
	switch err := db.QueryRow(`SELECT value FROM server_meta WHERE key=$1`, encKeyMetaKey).Scan(&v); err {
	case nil:
		return v
	case sql.ErrNoRows:
		return ""
	default:
		t.Fatalf("reading the fingerprint: %v", err)
		return ""
	}
}

// ── The fingerprint ───────────────────────────────────────────────────────

func TestEncKeyFingerprintIsStableAndDistinct(t *testing.T) {
	a := []byte("0123456789abcdef0123456789abcdef")
	b := []byte("0123456789abcdef0123456789abcdeF") // one bit of difference

	if encKeyFingerprint(a) != encKeyFingerprint(a) {
		t.Error("the fingerprint of one key is unstable — the server would never start")
	}
	if encKeyFingerprint(a) == encKeyFingerprint(b) {
		t.Error("different keys gave one fingerprint — a substitution would go unnoticed")
	}
	if got := len(encKeyFingerprint(a)); got != 16 {
		t.Errorf("fingerprint length %d, expected 16", got)
	}
}

// The fingerprint lives in the database, and a dump must not help decrypt it.
func TestEncKeyFingerprintDoesNotLeakKey(t *testing.T) {
	key := []byte("supersecretsupersecretsupersecre")
	fp := encKeyFingerprint(key)
	if strings.Contains(fp, string(key[:8])) {
		t.Fatal("the fingerprint contains a fragment of the key")
	}
	// The alphabet is telling too: hex cannot contain arbitrary characters from
	// the key.
	if strings.Trim(fp, "0123456789abcdef") != "" {
		t.Fatalf("the fingerprint is not hex: %q", fp)
	}
}

// ── Behaviour at start-up ─────────────────────────────────────────────────

// The first start: record it and let the server up.
func TestEncKeyGuardFirstRunRemembers(t *testing.T) {
	setupEncKeyGuard(t)
	clearEncKeyRow(t)

	fp := encKeyFingerprint([]byte("first-run-key"))
	if _, ok := encKeyGuardStatus(fp, ""); !ok {
		t.Fatal("the first start must not be blocked — there is nothing to compare with")
	}
	if got := storedFingerprint(t); got != fp {
		t.Fatalf("the fingerprint was not recorded: %q", got)
	}
}

// The same key: let it up and rewrite nothing.
func TestEncKeyGuardSameKeyPasses(t *testing.T) {
	setupEncKeyGuard(t)
	clearEncKeyRow(t)

	fp := encKeyFingerprint([]byte("steady-key"))
	encKeyGuardStatus(fp, "")

	stored, ok := encKeyGuardStatus(fp, "")
	if !ok {
		t.Fatal("an unchanged key was blocked — the server would not start at all")
	}
	if stored != fp {
		t.Fatalf("a foreign fingerprint came back: %q", stored)
	}
}

// THE MAIN POINT: a different key without confirmation is a refusal.
func TestEncKeyGuardRejectsChangedKey(t *testing.T) {
	setupEncKeyGuard(t)
	clearEncKeyRow(t)

	old := encKeyFingerprint([]byte("original-key"))
	encKeyGuardStatus(old, "")

	fresh := encKeyFingerprint([]byte("replacement-key"))
	stored, ok := encKeyGuardStatus(fresh, "")
	if ok {
		t.Fatal("the server would start with a FOREIGN key — old messages would turn to rubbish")
	}
	if stored != old {
		t.Errorf("the refusal names the wrong fingerprint: %q, expected %q", stored, old)
	}
	// A refusal must change nothing: the data is still intact, and restoring the
	// previous key must be enough.
	if got := storedFingerprint(t); got != old {
		t.Fatalf("the refusal rewrote the fingerprint to %q — restoring the key would no longer help", got)
	}
}

// A confirmation is accepted only with the correct fingerprint.
func TestEncKeyGuardAckMustMatch(t *testing.T) {
	setupEncKeyGuard(t)
	clearEncKeyRow(t)

	old := encKeyFingerprint([]byte("original-key"))
	encKeyGuardStatus(old, "")
	fresh := encKeyFingerprint([]byte("replacement-key"))

	// Wrong: empty, random, the old fingerprint, a truncated new one.
	for _, ack := range []string{"", "yes", "true", old, fresh[:8], fresh + "x"} {
		if _, ok := encKeyGuardStatus(fresh, ack); ok {
			t.Fatalf("confirmation %q was accepted — a typo in the key would pass unnoticed", ack)
		}
	}
	if got := storedFingerprint(t); got != old {
		t.Fatalf("a failed confirmation rewrote the fingerprint: %q", got)
	}

	// The correct one, with surrounding spaces as it arrives from systemd.
	if _, ok := encKeyGuardStatus(fresh, "  "+fresh+"\n"); !ok {
		t.Fatal("a correct confirmation was rejected — a deliberate key change is impossible")
	}
	if got := storedFingerprint(t); got != fresh {
		t.Fatalf("after the confirmation the database holds %q, expected %q", got, fresh)
	}
	// And the next start no longer needs a confirmation.
	if _, ok := encKeyGuardStatus(fresh, ""); !ok {
		t.Fatal("after a confirmation the server must start without one")
	}
}

// The fingerprint comes from SERVER_ENC_KEY itself, not from something else.
func TestCheckEncKeyUnchangedUsesConfiguredKey(t *testing.T) {
	setupEncKeyGuard(t)
	clearEncKeyRow(t)

	// encKey is stubbed by stubKeys inside setupIntegration — so what is checked
	// is precisely that the guard looks at the configured key rather than at
	// something of its own.
	os.Unsetenv("SERVER_ENC_KEY_ACK")
	checkEncKeyUnchanged()
	if got, want := storedFingerprint(t), encKeyFingerprint(encKey()); got != want {
		t.Fatalf("the recorded fingerprint is %q while the key gives %q", got, want)
	}
	// A repeat start with the same key must pass silently.
	checkEncKeyUnchanged()
}
