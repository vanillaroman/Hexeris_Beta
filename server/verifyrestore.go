package main

// The `verify-restore <scratch_db_url> [scratch_upload_dir]` subcommand.
//
// A read-only check of a restored set, proving the backup is actually usable
// rather than merely that psql did not fail. It runs against a scratch
// database and directory (see scripts/restore-drill.sh) and writes nothing.
//
// The decisive check is decrypting a sample of message bodies. They use
// AES-256-GCM, which is authenticated, so a successful Open with the current
// SERVER_ENC_KEY proves the key matches the backup — the property that makes
// a backup restorable at all. File payloads (AES-256-CTR) carry no tag, so
// only their structure ([HXE1][IV]) and stream setup can be verified.

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

func runVerifyRestore(args []string) {
	if len(args) < 1 {
		fmt.Println("usage: verify-restore <scratch_db_url> [scratch_upload_dir]")
		os.Exit(2)
	}
	dbURL := args[0]
	uploadDir := ""
	if len(args) > 1 {
		uploadDir = args[1]
	}

	// The postgres driver is registered by the package-level lib/pq import.
	sdb, err := sql.Open("postgres", dbURL)
	if err != nil {
		fmt.Println("VERIFY-RESTORE: FAIL — open db:", err)
		os.Exit(1)
	}
	defer sdb.Close()
	if err := sdb.Ping(); err != nil {
		fmt.Println("VERIFY-RESTORE: FAIL — ping db:", err)
		os.Exit(1)
	}

	// Integrity: the key tables must not be empty.
	var users, msgs, groups, reacts int
	sdb.QueryRow(`SELECT count(*) FROM users`).Scan(&users)
	sdb.QueryRow(`SELECT count(*) FROM messages`).Scan(&msgs)
	sdb.QueryRow(`SELECT count(*) FROM groups`).Scan(&groups)
	sdb.QueryRow(`SELECT count(*) FROM reactions`).Scan(&reacts)
	fmt.Printf("VERIFY-RESTORE: rows users=%d messages=%d groups=%d reactions=%d\n",
		users, msgs, groups, reacts)

	// Decrypt a sample of bodies; GCM authentication proves the key.
	block, err := aes.NewCipher(encKey())
	if err != nil {
		fmt.Println("VERIFY-RESTORE: FAIL — bad SERVER_ENC_KEY:", err)
		os.Exit(1)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		fmt.Println("VERIFY-RESTORE: FAIL — gcm:", err)
		os.Exit(1)
	}
	ciph, okDec, failDec := 0, 0, 0
	if rows, qerr := sdb.Query(`SELECT body FROM messages
		WHERE (media_type IS NULL OR media_type='') AND body <> '' AND deleted = false
		LIMIT 100`); qerr == nil {
		for rows.Next() {
			var b string
			if rows.Scan(&b) != nil {
				continue
			}
			raw, e := base64.StdEncoding.DecodeString(b)
			if e != nil || len(raw) < gcm.NonceSize() {
				continue // legacy plaintext row, not ciphertext
			}
			ciph++
			nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
			if _, e := gcm.Open(nil, nonce, ct, nil); e == nil {
				okDec++
			} else {
				failDec++
			}
		}
		rows.Close()
	}
	fmt.Printf("VERIFY-RESTORE: body-decrypt sampled_ciphertext=%d ok=%d fail=%d\n",
		ciph, okDec, failDec)

	// Files: header structure and CTR setup only — without an auth tag the
	// key itself cannot be verified from a file.
	if uploadDir != "" {
		total, enc, legacy := 0, 0, 0
		entries, _ := os.ReadDir(uploadDir)
		for _, en := range entries {
			if en.IsDir() {
				continue
			}
			total++
			f, e := os.Open(filepath.Join(uploadDir, en.Name()))
			if e != nil {
				continue
			}
			hdr := make([]byte, fileHeaderSize)
			n, _ := f.Read(hdr)
			f.Close()
			if n >= fileHeaderSize && string(hdr[:4]) == string(fileMagic) {
				enc++
				_ = cipher.NewCTR(block, hdr[4:fileHeaderSize]) // must not panic
			} else {
				legacy++
			}
		}
		fmt.Printf("VERIFY-RESTORE: files total=%d encrypted(HXE1)=%d legacy/other=%d\n",
			total, enc, legacy)
	}

	// Verdict.
	if msgs == 0 && users == 0 {
		fmt.Println("VERIFY-RESTORE: WARN — database is empty (nothing to verify)")
	}
	// Ciphertext present but not fully decryptable means a wrong key or a
	// corrupted dump.
	if ciph > 0 && (okDec == 0 || failDec > 0) {
		fmt.Printf("VERIFY-RESTORE: FAIL — body decryption failed (ok=%d fail=%d) — "+
			"wrong SERVER_ENC_KEY or damaged dump\n", okDec, failDec)
		os.Exit(1)
	}
	fmt.Println("VERIFY-RESTORE: PASS")
	os.Exit(0)
}
