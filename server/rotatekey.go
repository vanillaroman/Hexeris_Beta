package main

// Changing SERVER_ENC_KEY while keeping the data: hexeris rotate-enc-key
//
// ═══ WHY ═════════════════════════════════════════════════════════════════
//
// Before this subcommand the "the key is compromised" scenario in
// docs/operations/DISASTER-RECOVERY.md ended with an honest "there is no tool,
// this needs manual work and downtime". So the security question "how do you
// rotate your encryption key" had no answer — and it is asked right after
// "and how do you encrypt".
//
// The neighbouring guard (enckeyguard.go) prevents an ACCIDENTAL key swap.
// This is how to change it DELIBERATELY without losing the correspondence.
//
// ═══ THE MAIN DIFFICULTY: RESTARTING AFTER AN INTERRUPTION ═══════════════
//
// Re-encrypting hundreds of thousands of rows and gigabytes of files is an
// operation that will be interrupted one day: the disk fills up, the operator
// hits Ctrl-C, the machine dies. A repeat run must FINISH the work rather than
// spoil what is done. A second pass over an already re-encrypted row with the
// old key would turn it to rubbish — exactly the kind of error that goes
// unnoticed until the first read.
//
// For message bodies and TOTP secrets the eras are easy to tell apart: AES-GCM
// is authenticated, so a row is simply tried with the new key first and the old
// one second. If neither fits, the row is not ours (legacy plaintext) and must
// not be touched.
//
// Files are harder: they use AES-CTR with no integrity tag (for the sake of
// Range requests, see filecrypt.go), and "did it decrypt correctly" cannot be
// computed. So files go through a journal in the enc_rotation table plus a
// temporary file:
//
//   1. write <name>.rotating in full (the original is untouched);
//   2. RECORD in the journal that the file is done;
//   3. rename over the original (rename is atomic).
//
// An interruption at any point resolves unambiguously:
//   • no journal entry → the original is still old and .rotating is
//     unfinished rubbish: delete it and redo;
//   • an entry and .rotating present → step 3 did not finish: complete the
//     rename;
//   • an entry and no .rotating → done.
//
// The "journal before rename" order was chosen precisely for this: the reverse
// would leave, after an interruption, a file on the new key with no entry —
// so a repeat pass would kill it.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// rotateBatch — how many rows are fetched per query. Not for speed: without a
// ceiling, selecting "all messages" pulls the whole correspondence into memory.
const rotateBatch = 500

// ─── Working with an explicitly passed key ───────────────────────────────
//
// The usual encryptBody/decryptBody take the key from the environment through
// encKey(). Here there are two keys at once, so explicit-key variants exist.

func encryptBodyWith(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// decryptBodyWith differs from decryptBody in the main thing: it reports an
// error HONESTLY. decryptBody deliberately returns the stored value as it is
// (so one broken row does not break the whole history), but here the decision
// is made on "it did not fit", and a silent failure would cost data.
func decryptBodyWith(key []byte, stored string) (string, bool) {
	if stored == "" {
		return "", false
	}
	data, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", false
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	if len(data) < gcm.NonceSize() {
		return "", false
	}
	pt, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return "", false
	}
	return string(pt), true
}

// ─── Entry point ─────────────────────────────────────────────────────────

type rotateStats struct {
	msgRotated, msgAlready, msgForeign int
	secRotated, secAlready, secForeign int
	fileRotated, fileAlready           int
	filePlain, fileFailed              int
}

func runRotateEncKey(args []string) {
	dry, yes := false, false
	for _, a := range args {
		switch a {
		case "--dry-run", "-n":
			dry = true
		case "--yes", "-y":
			yes = true
		default:
			log.Fatalf("unknown argument %q (allowed: --dry-run, --yes)", a)
		}
	}

	oldKey, newKey := rotateLoadKeys()
	oldFP, newFP := encKeyFingerprint(oldKey), encKeyFingerprint(newKey)

	// The start-up fingerprint check must be SKIPPED here: after a successful
	// run the database holds the new key's fingerprint while the operator's
	// SERVER_ENC_KEY is still the old one — and a repeat run (finishing after
	// an interruption, writing the fingerprint) would hit a wall put up for a
	// different case.
	skipEncKeyGuard = true
	initDB()
	initEncKeyGuardSchema()
	initRotationSchema()

	// The key the database is encrypted with RIGHT NOW must match the one
	// declared as old. Otherwise the re-encryption would "succeed" without
	// touching a single row: none of them would decrypt.
	var stored string
	switch err := db.QueryRow(`SELECT value FROM server_meta WHERE key=$1`, encKeyMetaKey).Scan(&stored); {
	case err == nil && stored == newFP:
		log.Printf("The database already holds the NEW key fingerprint (%s).", newFP)
		log.Printf("The rotation appears to have run already. This run will continue")
		log.Printf("and finish whatever the interruption left — that is safe.")
	case err == nil && stored != oldFP:
		log.Fatalf(`FATAL: the database is encrypted with a key other than the one declared old.

  fingerprint in the database: %s
  fingerprint of SERVER_ENC_KEY: %s

Re-encrypting with the wrong old key will not spoil the data — but it will do
nothing either: no row will decrypt. Find the real current key.`,
			stored, oldFP)
	case err != nil:
		log.Printf("the key fingerprint could not be read (%v) — continuing, but there is nothing to compare with", err)
	}

	log.Printf("old key: %s   new key: %s", oldFP, newFP)
	if dry {
		log.Println("DRY RUN: nothing is written.")
	} else if !yes {
		log.Fatal(`FATAL: re-encryption changes the data irreversibly.

Before running it:

  1. Stop the server: systemctl stop hexeris
     On a running server some rows appear during the run itself — encrypted
     with the old key — and stay unreadable.

  2. Take a backup: hexeris backup
     (docs/operations/BACKUP.md). The run is restartable, but a copy is what
     separates a failed rotation from lost correspondence.

  3. Check the volume of work: hexeris rotate-enc-key --dry-run

Then repeat with --yes.`)
	}

	st := &rotateStats{}
	rotateMessages(oldKey, newKey, dry, st)
	rotateTOTPSecrets(oldKey, newKey, dry, st)
	rotateFiles(oldKey, newKey, dry, st)

	log.Println("─────────────────────────────────────────────")
	log.Printf("messages:    re-encrypted %d, already on the new key %d, foreign/plaintext %d",
		st.msgRotated, st.msgAlready, st.msgForeign)
	log.Printf("2FA secrets: re-encrypted %d, already on the new key %d, unreadable %d",
		st.secRotated, st.secAlready, st.secForeign)
	log.Printf("files:       re-encrypted %d, done earlier %d, unencrypted %d, failed %d",
		st.fileRotated, st.fileAlready, st.filePlain, st.fileFailed)

	if dry {
		log.Println("Dry run finished. No data was changed.")
		return
	}
	if st.fileFailed > 0 {
		// The fingerprint is NOT updated: otherwise the server would start with
		// the new key while some files stayed on the old one and quietly stopped
		log.Fatalf(`FATAL: %d files were not re-encrypted (the reasons are above).

The key fingerprint in the database was NOT updated, and SERVER_ENC_KEY must
not be changed yet: the server would start with the new key and these files
would stop opening. Find the cause and run the rotation again — what is
already done is skipped.`, st.fileFailed)
	}

	if _, err := db.Exec(
		`INSERT INTO server_meta(key, value) VALUES($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, set_at=NOW()`,
		encKeyMetaKey, newFP); err != nil {
		log.Fatalf("FATAL: the data was re-encrypted but the new key fingerprint was not written: %v\n"+
			"Run the rotation again — it will skip the finished work and write the fingerprint.", err)
	}

	log.Println("─────────────────────────────────────────────")
	log.Printf("Done. Now replace SERVER_ENC_KEY with the new one and start the server.")
	log.Printf("New key fingerprint: %s — the server checks it at start-up.", newFP)
	log.Printf("DO NOT DISCARD THE OLD KEY until you are sure the history reads:")
	log.Printf("it is still needed for backups taken BEFORE this rotation.")
}

// rotateLoadKeys reads and validates both keys.
func rotateLoadKeys() (oldKey, newKey []byte) {
	parse := func(name string) []byte {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			log.Fatalf("FATAL: %s is not set", name)
		}
		k, err := base64.StdEncoding.DecodeString(raw)
		if err != nil || len(k) != 32 {
			log.Fatalf("FATAL: %s must be 32 bytes in base64 (`openssl rand -base64 32`)", name)
		}
		return k
	}
	oldKey = parse("SERVER_ENC_KEY")
	newKey = parse("SERVER_ENC_KEY_NEW")
	if encKeyFingerprint(oldKey) == encKeyFingerprint(newKey) {
		log.Fatal("FATAL: SERVER_ENC_KEY_NEW equals the old key — there is nothing to change")
	}
	return oldKey, newKey
}

func initRotationSchema() {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS enc_rotation (
		kind    TEXT NOT NULL,
		item    TEXT NOT NULL,
		key_fp  TEXT NOT NULL,
		done_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (kind, item, key_fp)
	)`); err != nil {
		log.Fatal("FATAL: cannot create the rotation journal: ", err)
	}
}

// ─── Message bodies ──────────────────────────────────────────────────────

func rotateMessages(oldKey, newKey []byte, dry bool, st *rotateStats) {
	log.Println("message bodies…")
	var cursor int64
	for {
		rows, err := db.Query(
			`SELECT seq, body FROM messages WHERE seq > $1 ORDER BY seq LIMIT $2`, cursor, rotateBatch)
		if err != nil {
			log.Fatal("FATAL: reading messages: ", err)
		}
		type item struct {
			seq  int64
			body string
		}
		var batch []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.seq, &it.body); err != nil {
				rows.Close()
				log.Fatal("FATAL: reading a message: ", err)
			}
			batch = append(batch, it)
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}
		cursor = batch[len(batch)-1].seq

		for _, it := range batch {
			// The NEW key first: if the row was already re-encrypted by a previous
			// (interrupted) run, a second pass with the old key would kill it.
			if _, ok := decryptBodyWith(newKey, it.body); ok {
				st.msgAlready++
				continue
			}
			plain, ok := decryptBodyWith(oldKey, it.body)
			if !ok {
				// Neither key fitted: either this is plaintext left over from
				// prehistoric times, or the row is broken. It must not be touched in
				// either case.
				st.msgForeign++
				continue
			}
			if dry {
				st.msgRotated++
				continue
			}
			enc, err := encryptBodyWith(newKey, plain)
			if err != nil {
				log.Fatal("FATAL: encrypting the new body: ", err)
			}
			if _, err := db.Exec(`UPDATE messages SET body=$2 WHERE seq=$1`, it.seq, enc); err != nil {
				log.Fatal("FATAL: writing the body seq=", it.seq, ": ", err)
			}
			st.msgRotated++
		}
		if (st.msgRotated+st.msgAlready+st.msgForeign)%10000 < rotateBatch {
			log.Printf("  … %d messages", st.msgRotated+st.msgAlready+st.msgForeign)
		}
	}
}

// ─── Second-factor secrets ───────────────────────────────────────────────

func rotateTOTPSecrets(oldKey, newKey []byte, dry bool, st *rotateStats) {
	log.Println("second-factor secrets…")
	rows, err := db.Query(`SELECT username, totp_secret FROM users WHERE totp_secret IS NOT NULL AND totp_secret <> ''`)
	if err != nil {
		// The column may be missing on a very old database — no reason to die.
		log.Println("  skipped:", err)
		return
	}
	type item struct{ user, secret string }
	var batch []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.user, &it.secret) == nil {
			batch = append(batch, it)
		}
	}
	rows.Close()

	for _, it := range batch {
		if _, ok := decryptBodyWith(newKey, it.secret); ok {
			st.secAlready++
			continue
		}
		plain, ok := decryptBodyWith(oldKey, it.secret)
		if !ok {
			// The secret is unreadable with either key. Reported by name: this
			// employee's second factor will stop working, and learning that now is
			// better than hearing it from them.
			log.Printf("  WARNING: the 2FA secret of user %q decrypts with neither key; "+
				"after the rotation they will need a second-factor reset", it.user)
			st.secForeign++
			continue
		}
		if dry {
			st.secRotated++
			continue
		}
		enc, err := encryptBodyWith(newKey, plain)
		if err != nil {
			log.Fatal("FATAL: encrypting the secret: ", err)
		}
		if _, err := db.Exec(`UPDATE users SET totp_secret=$2 WHERE username=$1`, it.user, enc); err != nil {
			log.Fatal("FATAL: writing the secret for ", it.user, ": ", err)
		}
		st.secRotated++
	}
}

// ─── Files ───────────────────────────────────────────────────────────────

func rotateFiles(oldKey, newKey []byte, dry bool, st *rotateStats) {
	dir := uploadDir()
	log.Println("files in", dir, "…")
	newFP := encKeyFingerprint(newKey)

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal("FATAL: reading the upload directory: ", err)
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".rotating") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		tmp := path + ".rotating"

		done := rotationDone("file", e.Name(), newFP)
		if done {
			// An interruption between the entry and the rename: finish the rename.
			if _, err := os.Stat(tmp); err == nil {
				if rerr := os.Rename(tmp, path); rerr != nil {
					log.Printf("  ERROR: %s — the move was not completed: %v", e.Name(), rerr)
					st.fileFailed++
					continue
				}
				log.Printf("  %s — move completed after an interruption", e.Name())
			}
			st.fileAlready++
			continue
		}
		// No entry: the original is still on the old key, and .rotating (if any)
		// is unfinished rubbish from a previous run.
		os.Remove(tmp)

		encrypted, err := fileIsEncrypted(path)
		if err != nil {
			log.Printf("  ERROR: %s — %v", e.Name(), err)
			st.fileFailed++
			continue
		}
		if !encrypted {
			// A legacy file uploaded before encryption was switched on. It is
			// served as it is anyway (see filecrypt.go) — no reason to touch it.
			st.filePlain++
			continue
		}
		if dry {
			st.fileRotated++
			continue
		}
		if err := rotateOneFile(path, tmp, oldKey, newKey); err != nil {
			log.Printf("  ERROR: %s — %v", e.Name(), err)
			os.Remove(tmp)
			st.fileFailed++
			continue
		}
		// The entry BEFORE the rename — see the explanation in the file header.
		if _, err := db.Exec(
			`INSERT INTO enc_rotation(kind, item, key_fp) VALUES('file',$1,$2) ON CONFLICT DO NOTHING`,
			e.Name(), newFP); err != nil {
			log.Printf("  ERROR: %s — the journal was not written: %v", e.Name(), err)
			os.Remove(tmp)
			st.fileFailed++
			continue
		}
		if err := os.Rename(tmp, path); err != nil {
			log.Printf("  ERROR: %s — the move failed: %v", e.Name(), err)
			st.fileFailed++
			continue
		}
		st.fileRotated++
	}
}

func rotationDone(kind, item, keyFP string) bool {
	var one int
	err := db.QueryRow(`SELECT 1 FROM enc_rotation WHERE kind=$1 AND item=$2 AND key_fp=$3`,
		kind, item, keyFP).Scan(&one)
	return err == nil
}

// fileIsEncrypted looks at the magic at the start of the file.
func fileIsEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, len(fileMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return false, nil // too short to be one of ours
		}
		return false, err
	}
	return string(head) == string(fileMagic), nil
}

// rotateOneFile writes the same payload into tmp under the new key.
// The original is untouched — the caller replaces it with an atomic rename.
func rotateOneFile(path, tmp string, oldKey, newKey []byte) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	header := make([]byte, fileHeaderSize)
	if _, err := io.ReadFull(src, header); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	oldIV := header[len(fileMagic):]

	oldBlock, err := aes.NewCipher(oldKey)
	if err != nil {
		return err
	}
	// A new IV rather than the old one: the (key, IV) pair must not repeat in
	// CTR, and although the key differs here, saving sixteen random bytes in an
	// operation that rewrites the whole file anyway is pointless.
	newIV := make([]byte, fileIVSize)
	if _, err := rand.Read(newIV); err != nil {
		return err
	}
	newBlock, err := aes.NewCipher(newKey)
	if err != nil {
		return err
	}

	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// Closing with an error check: an unclosed file may turn out to be
	// unfinished, and we are about to rename it over the original.
	closed := false
	defer func() {
		if !closed {
			dst.Close()
		}
	}()

	if _, err := dst.Write(fileMagic); err != nil {
		return err
	}
	if _, err := dst.Write(newIV); err != nil {
		return err
	}
	// Decrypting with the old stream and encrypting with the new goes in chunks:
	// files can be up to 60 MB, and there is no reason to hold them in memory.
	plain := cipher.StreamReader{S: cipher.NewCTR(oldBlock, oldIV), R: src}
	sw := &cipher.StreamWriter{S: cipher.NewCTR(newBlock, newIV), W: dst}
	if _, err := io.Copy(sw, plain); err != nil {
		return err
	}
	// fsync before the rename: without it, after a power loss the file would be
	// zero-sized while the journal claimed everything was done.
	if err := dst.Sync(); err != nil {
		return err
	}
	closed = true
	return dst.Close()
}
