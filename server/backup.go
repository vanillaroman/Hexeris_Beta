package main

// Database backups via pg_dump.
//
// Disabled by default: on a small host, automatic backups eat the disk, so
// this is a capability rather than a running process. The scheduler starts
// only with DB_BACKUP_ENABLED=true, and `./hexeris backup` takes a single
// backup by hand without scheduling anything.
//
// Environment:
//
//	DB_BACKUP_ENABLED=true        enable the periodic scheduler
//	DB_BACKUP_DIR=…               where to write (default ./backups)
//	DB_BACKUP_INTERVAL_HOURS=24   scheduler period
//	DB_BACKUP_KEEP=7              how many recent dumps to keep
//
// A backup set shares one timestamp across both files:
//
//	hexeris-<stamp>.sql.gz        database dump (pg_dump | gzip)
//	hexeris-files-<stamp>.tar.gz  attachments from UPLOAD_DIR (tar+gzip)
//
// Files are written 0600 in a 0700 directory. Message bodies and attachments
// are already encrypted at rest with SERVER_ENC_KEY. Restoring is documented
// in docs/BACKUP.md:
//
//	gunzip -c hexeris-<stamp>.sql.gz | psql "$DATABASE_URL"
//	tar -xzf hexeris-files-<stamp>.tar.gz -C "$UPLOAD_DIR"

import (
	"compress/gzip"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func backupEnabled() bool { return os.Getenv("DB_BACKUP_ENABLED") == "true" }

// backupIntervalHours is used by both the scheduler and the health check,
// which needs it to decide whether the last backup is stale.
func backupIntervalHours() int {
	if v, err := strconv.Atoi(os.Getenv("DB_BACKUP_INTERVAL_HOURS")); err == nil && v > 0 {
		return v
	}
	return 24
}

func backupDir() string { return getEnvOrDefault("DB_BACKUP_DIR", "backups") }

// A ceiling on the off-site upload: without it a hung network holds the
// scheduler goroutine until the next interval.
const offsiteTimeout = 30 * time.Minute

// backupStatus is the last known backup state, surfaced in /admin/metrics.
// Kept only in logs, success and failure leave an operator unable to answer
// when the last backup ran and whether it reached off-site storage — and
// silently failing backups are then discovered when they are needed.
type backupStatus struct {
	mu        sync.Mutex
	lastAt    time.Time
	lastOK    bool
	lastErr   string
	offsiteAt time.Time
	offsiteOK bool
}

var backupState backupStatus

func (b *backupStatus) record(path string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAt = time.Now()
	b.lastOK = err == nil
	if err != nil {
		b.lastErr = err.Error()
	} else {
		b.lastErr = ""
	}
}

func (b *backupStatus) recordOffsite(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.offsiteAt = time.Now()
	b.offsiteOK = ok
}

// snapshot renders the state for /admin/metrics.
func (b *backupStatus) snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]any{"enabled": backupEnabled()}
	if b.lastAt.IsZero() {
		out["last_run"] = nil
		out["age_hours"] = nil
	} else {
		out["last_run"] = b.lastAt.UTC().Format(time.RFC3339)
		out["age_hours"] = int(time.Since(b.lastAt).Hours())
		out["last_ok"] = b.lastOK
		if b.lastErr != "" {
			out["last_error"] = b.lastErr
		}
	}
	if !b.offsiteAt.IsZero() {
		out["offsite_last_run"] = b.offsiteAt.UTC().Format(time.RFC3339)
		out["offsite_ok"] = b.offsiteOK
	} else if os.Getenv("DB_BACKUP_OFFSITE_CMD") == "" {
		// A machine token, not a sentence: the admin panel renders the
		// human-readable text, the server reports state.
		out["offsite"] = "not_configured"
	}
	return out
}

// startBackupJanitor starts periodic backups only when explicitly enabled,
// and otherwise does nothing at all.
func startBackupJanitor() {
	if !backupEnabled() {
		return
	}
	hours := backupIntervalHours()
	interval := time.Duration(hours) * time.Hour
	log.Printf("DB backup scheduler ENABLED — every %dh into %s", hours, backupDir())

	safeGo("backupJanitor", func() {
		// Catch-up backup at startup. Sleeping the interval first means a
		// service restarted more often than the interval — deploys, config
		// changes — never backs up at all. Checking the newest file on disk
		// and dumping immediately when it is older than the interval (or
		// absent) closes that gap.
		if age, ok := newestBackupAge(backupDir()); !ok || age >= interval {
			if path, err := runBackupOnce(); err != nil {
				backupState.record("", err)
				log.Println("startup backup failed:", err)
			} else {
				log.Println("startup backup written:", path)
			}
		}
		for {
			time.Sleep(interval)
			if path, err := runBackupOnce(); err != nil {
				backupState.record("", err)
				log.Println("db backup failed:", err)
			} else {
				log.Println("db backup written:", path)
			}
		}
	})
}

// runBackupOnce writes one gzipped pg_dump, prunes older sets according to
// DB_BACKUP_KEEP and returns the path it created.
func runBackupOnce() (string, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return "", fmt.Errorf("DATABASE_URL is not set")
	}
	dir := backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	name := "hexeris-" + stamp + ".sql.gz"
	path := filepath.Join(dir, name)
	tmp := path + ".partial"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 — path comes from backupDir
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)

	// pg_dump connects by DSN itself.
	cmd := exec.Command("pg_dump", "--no-owner", "--no-privileges", dsn) // #nosec G204 — dsn comes from the operator's env
	cmd.Stdout = gz
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()
	gzErr := gz.Close()
	closeErr := f.Close()

	if runErr != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("pg_dump: %w (is pg_dump installed and DATABASE_URL reachable?)", runErr)
	}
	if gzErr != nil || closeErr != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("write backup: %v / %v", gzErr, closeErr)
	}
	if err := os.Rename(tmp, path); err != nil { // publish the finished file atomically
		os.Remove(tmp)
		return "", err
	}

	// Attachments go into a separate archive sharing the timestamp, so dump
	// and files form one consistent set. A failure here does not invalidate
	// the dump already taken: a partial backup beats none.
	if fpath, ferr := backupFiles(dir, stamp); ferr != nil {
		log.Println("file backup failed (DB dump ok):", ferr)
	} else if fpath != "" {
		log.Println("files backup written:", fpath)
	}

	pruneOldBackups(dir)
	backupState.record(path, nil)
	runOffsiteCopy(dir, path)
	return path, nil
}

// runOffsiteCopy sends the fresh backup off this machine.
//
// Without it every backup sits on the same disk as production, so losing the
// host — disk failure, a deleted instance, ransomware — takes the data and
// its copies together. This is the central gap in disaster recovery, and the
// operator closes it by setting DB_BACKUP_OFFSITE_CMD.
//
// A command rather than a built-in rsync/rclone/S3 client: every deployment
// has its own destination, and none of them justify linking a cloud SDK into
// the binary. Paths are passed through the environment (BACKUP_FILE,
// BACKUP_FILES_ARCHIVE, BACKUP_DIR) rather than string substitution, which
// keeps quoting and spaces out of the picture.
//
// Example:
//
//	DB_BACKUP_OFFSITE_CMD='rclone copy "$BACKUP_FILE" remote:hexeris-backups/'
func runOffsiteCopy(dir, dbPath string) {
	cmdline := strings.TrimSpace(os.Getenv("DB_BACKUP_OFFSITE_CMD"))
	if cmdline == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), offsiteTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline) // #nosec G204 — command comes from the operator's env
	cmd.Env = append(os.Environ(),
		"BACKUP_FILE="+dbPath,
		"BACKUP_FILES_ARCHIVE="+strings.Replace(dbPath, "hexeris-", "hexeris-files-", 1),
		"BACKUP_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// An off-site failure does not invalidate the backup — the local
		// copy exists — but it must stay visible, or "we have backups"
		// quietly becomes "we have backups on the machine that died".
		backupState.recordOffsite(false)
		log.Printf("OFF-SITE backup FAILED: %v — %s", err, strings.TrimSpace(string(out)))
		return
	}
	backupState.recordOffsite(true)
	log.Println("off-site copy ok:", filepath.Base(dbPath))
}

// backupFiles archives UPLOAD_DIR into hexeris-files-<stamp>.tar.gz. A missing
// or unreadable directory is not an error. The files are already encrypted at
// rest, so the archive is safe to keep beside the dump.
func backupFiles(dir, stamp string) (string, error) {
	src := os.Getenv("UPLOAD_DIR")
	if src == "" {
		return "", nil
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return "", nil // no directory, nothing to archive
	}
	path := filepath.Join(dir, "hexeris-files-"+stamp+".tar.gz")
	tmp := path + ".partial"
	// "-C src ." stores relative paths so a restore can target any
	// directory: tar -xzf FILE -C /new/upload.
	cmd := exec.Command("tar", "-czf", tmp, "-C", src, ".") // #nosec G204 — src comes from the operator's env
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("tar %s: %w (is tar installed?)", src, err)
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// pruneOldBackups keeps only the DB_BACKUP_KEEP most recent sets.
func pruneOldBackups(dir string) {
	keep := 7
	if v, err := strconv.Atoi(os.Getenv("DB_BACKUP_KEEP")); err == nil && v > 0 {
		keep = v
	}
	// Dumps and file archives are pruned independently.
	for _, pat := range []string{"hexeris-*.sql.gz", "hexeris-files-*.tar.gz"} {
		entries, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil || len(entries) <= keep {
			continue
		}
		sort.Strings(entries) // UTC timestamps sort lexicographically
		for _, old := range entries[:len(entries)-keep] {
			if err := os.Remove(old); err != nil {
				log.Println("prune backup:", err)
			}
		}
	}
}

// newestBackupAge returns the age of the most recent dump; ok=false means
// there are no backups at all, or the directory is unreadable.
func newestBackupAge(dir string) (time.Duration, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "hexeris-") ||
			!strings.HasSuffix(e.Name(), ".sql.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	return time.Since(newest), true
}
