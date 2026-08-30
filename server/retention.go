package main

// Retention: automatic deletion of old data.
//
// IMPORTANT: OFF by default — enabled only by an explicit
// RETENTION_ENABLED=true. This is a corporate messenger, and "it deleted
// itself" without the owner knowing is worse than a large database. Mirrors
//
// Environment variables:
//   RETENTION_ENABLED=true        — enable the scheduler
//   RETENTION_MESSAGE_DAYS=365    — delete messages older than N days (0 = keep)
//   RETENTION_AUDIT_DAYS=180      — delete admin_audit rows older than N days (0 = keep)
//   RETENTION_INTERVAL_HOURS=24   — how often to run a pass
//
// What is NEVER deleted: users, groups, group membership, push subscriptions.
// Only messages are cleared (together with their reactions and orphaned
// attachment files) and the administrator action log.
//
// Take a backup before enabling it (docs/operations/BACKUP.md): it is irreversible.

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func retentionEnabled() bool { return os.Getenv("RETENTION_ENABLED") == "true" }

func startRetentionJanitor() {
	if !retentionEnabled() {
		return
	}
	msgDays := getEnvInt("RETENTION_MESSAGE_DAYS", 365)
	auditDays := getEnvInt("RETENTION_AUDIT_DAYS", 180)
	every := time.Duration(getEnvInt("RETENTION_INTERVAL_HOURS", 24)) * time.Hour
	log.Printf("RETENTION enabled: messages older than %dd, audit older than %dd, every %s",
		msgDays, auditDays, every)
	safeGo("retentionJanitor", func() {
		for range time.Tick(every) {
			runRetentionOnce(msgDays, auditDays)
		}
	})
}

// runRetentionOnce — one pass. Returns the number of deleted messages, audit
// records and files (for the logs and the tests).
func runRetentionOnce(msgDays, auditDays int) (msgs, audits, files int) {
	// The stale link preview cache goes along with it: giving it a schedule of
	// its own would mean a second moving part for a table that already bounds
	// its own size.
	if n := cleanUnfurlCache(); n > 0 {
		log.Printf("retention: deleted %d link preview cache rows", n)
	}
	if msgDays > 0 {
		// The attachment paths that will go with the messages are collected first:
		// after the DELETE there is nowhere left to learn them, and the files would
		// settle on the disk forever.
		var orphans []string
		rows, err := db.Query(`SELECT body FROM messages
			WHERE created_at < NOW() - MAKE_INTERVAL(days => $1)
			  AND COALESCE(media_type,'') <> ''`, msgDays)
		if err == nil {
			for rows.Next() {
				var body string
				if rows.Scan(&body) == nil && strings.HasPrefix(body, "/files/") {
					orphans = append(orphans, body)
				}
			}
			rows.Close()
		}

		// Reactions live in a separate table with no FK to messages, so they are
		// cleared explicitly, or they outlive their messages forever.
		db.Exec(`DELETE FROM reactions WHERE msg_id IN (
			SELECT id FROM messages WHERE created_at < NOW() - MAKE_INTERVAL(days => $1))`, msgDays)

		res, err := db.Exec(`DELETE FROM messages
			WHERE created_at < NOW() - MAKE_INTERVAL(days => $1)`, msgDays)
		if err != nil {
			log.Println("retention: delete messages failed:", err)
		} else if n, e := res.RowsAffected(); e == nil {
			msgs = int(n)
		}

		// The files are deleted AFTER the rows are deleted successfully: otherwise a
		// database error would leave messages in the history with broken attachments.
		if msgs > 0 {
			for _, u := range orphans {
				name := filepath.Base(u)
				if name == "." || name == "/" || strings.Contains(name, "..") {
					continue
				}
				if os.Remove(filepath.Join(uploadDir(), name)) == nil {
					files++
				}
			}
		}
	}

	if auditDays > 0 {
		res, err := db.Exec(`DELETE FROM admin_audit
			WHERE created_at < NOW() - MAKE_INTERVAL(days => $1)`, auditDays)
		if err != nil {
			log.Println("retention: delete audit failed:", err)
		} else if n, e := res.RowsAffected(); e == nil {
			audits = int(n)
		}
	}

	if msgs > 0 || audits > 0 || files > 0 {
		log.Printf("retention: removed %d messages, %d audit rows, %d files", msgs, audits, files)
	}
	return msgs, audits, files
}
