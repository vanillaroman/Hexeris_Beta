package main

// Retention: automatic removal of old data.
//
// Disabled unless RETENTION_ENABLED=true. In a corporate messenger, data
// disappearing without the owner's knowledge is worse than a growing
// database, so this never turns itself on.
//
// Environment:
//
//	RETENTION_ENABLED=true        enable the scheduler
//	RETENTION_MESSAGE_DAYS=365    delete messages older than N days (0 = keep)
//	RETENTION_AUDIT_DAYS=180      delete admin_audit rows older than N days
//	RETENTION_INTERVAL_HOURS=24   how often to run a pass
//
// Never removed: users, groups, group membership, push subscriptions. Only
// messages (with their reactions and orphaned attachments) and the admin
// action log are pruned. The operation is irreversible — take a backup first
// (docs/BACKUP.md).

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

// runRetentionOnce performs a single pass and reports what it removed.
func runRetentionOnce(msgDays, auditDays int) (msgs, audits, files int) {
	if msgDays > 0 {
		// Collect attachment paths before the DELETE: afterwards there is
		// nothing left to derive them from and the files would stay on disk
		// forever.
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

		// Reactions live in their own table without an FK to messages, so
		// they have to be pruned explicitly or they outlive their messages.
		db.Exec(`DELETE FROM reactions WHERE msg_id IN (
			SELECT id FROM messages WHERE created_at < NOW() - MAKE_INTERVAL(days => $1))`, msgDays)

		res, err := db.Exec(`DELETE FROM messages
			WHERE created_at < NOW() - MAKE_INTERVAL(days => $1)`, msgDays)
		if err != nil {
			log.Println("retention: delete messages failed:", err)
		} else if n, e := res.RowsAffected(); e == nil {
			msgs = int(n)
		}

		// Files go only after the rows are gone: on a database error the
		// history would otherwise keep messages with broken attachments.
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
