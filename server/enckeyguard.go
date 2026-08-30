package main

// Guarding against a substituted at-rest encryption key.
//
// ═══ WHAT HAPPENED WITHOUT IT ═════════════════════════════════════════════
//
// SERVER_ENC_KEY encrypts message bodies and files. If it was changed, the
// server came up as if nothing had happened: old records could no longer be
// decrypted (on failure decryptBody returns the stored value as it is, so the
// user sees base64 rubbish instead of their conversation) while new ones were
// written with the new key. The database ends up holding two encryption eras,
// and the longer the server runs the harder they are to separate — even though
// the data is still intact and recoverable if the previous key comes back.
//
// The key's fingerprint is therefore kept in the database and checked at every
// start. If it does not match, the server does NOT come up and says what to do.
// Refusing here is plainly cheaper than corrupting quietly.
//
// ═══ WHY A FINGERPRINT RATHER THAN THE KEY ════════════════════════════════
//
// What is stored is a SHA-256 of the key, not the key itself: a database dump
// must not hand over the means to decrypt its own contents. Eight characters
// are enough to tell one key from another and useless for guessing one.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log"
	"os"
	"strings"
)

func initEncKeyGuardSchema() {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		set_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	if err != nil {
		log.Println("server_meta:", err)
	}
}

// encKeyFingerprint — what goes into the database for comparison.
func encKeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:16]
}

const encKeyMetaKey = "enc_key_fingerprint"

// encKeyGuardStatus compares the fingerprint against the database and answers
// whether starting is allowed. It is deliberately separate from
// checkEncKeyUnchanged: log.Fatal ends the process, and a decision can only be
// tested when it is separated from the way it is carried out.
//
// Returns the fingerprint recorded in the database and the verdict.
func encKeyGuardStatus(fp, ack string) (stored string, ok bool) {
	err := db.QueryRow(`SELECT value FROM server_meta WHERE key=$1`, encKeyMetaKey).Scan(&stored)
	switch {
	case err == sql.ErrNoRows:
		// The first start with this guard (or an empty database) — record it.
		// ON CONFLICT DO NOTHING: two instances starting at the same time must
		// not fight over the row.
		if _, err := db.Exec(
			`INSERT INTO server_meta(key, value) VALUES ($1,$2) ON CONFLICT (key) DO NOTHING`,
			encKeyMetaKey, fp); err != nil {
			log.Println("could not record the key fingerprint:", err)
		}
		return fp, true
	case err != nil:
		// The comparison failed — but failing to start is not an option either:
		// one unavailable table is no reason to stop a working messenger.
		log.Println("key fingerprint check skipped:", err)
		return "", true
	case stored == fp:
		return stored, true
	}

	// The key is DIFFERENT. The only legitimate way past this is an explicit
	// confirmation from the administrator naming the new fingerprint: that way
	// a typo in the variable does not pass, and a deliberate act does.
	if strings.TrimSpace(ack) != fp {
		return stored, false
	}
	if _, err := db.Exec(`UPDATE server_meta SET value=$2, set_at=NOW() WHERE key=$1`,
		encKeyMetaKey, fp); err != nil {
		// The confirmation was accepted but could not be recorded: the next
		// start would run into the same wall again, this time without the data
		// that could still have been saved. That is a refusal, not a warning.
		log.Fatalf("FATAL: could not write the new key fingerprint: %v", err)
	}
	log.Printf("WARNING: SERVER_ENC_KEY has been changed and confirmed by an administrator (fingerprint %s). "+
		"Anything encrypted with the previous key is NOT readable with this one.", fp)
	return stored, true
}

// skipEncKeyGuard disables the check. It is set by exactly one subcommand —
// rotate-enc-key — which by its nature has to work at the moment when the key
// in the environment and the fingerprint in the database do NOT match.
var skipEncKeyGuard bool

// checkEncKeyUnchanged compares the key against the one the database is already
// encrypted with.
//
// Called at start-up, AFTER initDB and BEFORE the server begins accepting
// requests.
func checkEncKeyUnchanged() {
	if skipEncKeyGuard {
		return
	}
	fp := encKeyFingerprint(encKey())
	stored, ok := encKeyGuardStatus(fp, os.Getenv("SERVER_ENC_KEY_ACK"))
	if ok {
		return
	}

	log.Fatalf(`FATAL: SERVER_ENC_KEY differs from the key this database was encrypted with.

  fingerprint in the database: %s
  fingerprint of the current:  %s

The server has been stopped DELIBERATELY. Starting with a different key would
make old messages and files unreadable (rubbish instead of text) while new ones
were written with the new key — leaving a mixture in the database that is next
to impossible to untangle afterwards. Right now the data is still intact.

What to do:

  1. Restore the previous SERVER_ENC_KEY. This is the ordinary case — the key
     was simply lost or mixed up during a move. Look for it in the backup of
     the service configuration.

  2. If the key really is being changed deliberately and losing the old data is
     accepted, confirm it by naming the new fingerprint explicitly:

         SERVER_ENC_KEY_ACK=%s

     The confirmation requires the fingerprint precisely so that a typo in the
     key itself does not pass unnoticed.`, stored, fp, fp)
}
