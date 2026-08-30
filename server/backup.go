package main

// Database backups (pg_dump).
//
// IMPORTANT: OFF by default. On a small VPS automatic backups eat the disk, so
// what lives here is a *capability*, not a process enabled by default. The
// scheduler starts only with DB_BACKUP_ENABLED=true. A one-off backup can be
// taken by hand with `./server backup` — that schedules nothing.
//
// Environment variables:
//   DB_BACKUP_ENABLED=true        — enable the periodic scheduler
//   DB_BACKUP_DIR=/var/backups/hexeris  — where to put them (default ./backups)
//   DB_BACKUP_INTERVAL_HOURS=24   — the scheduler period (default 24)
//   DB_BACKUP_KEEP=7              — how many recent dumps to keep (default 7)
//
// A backup set (one timestamp for both files):
//   hexeris-<stamp>.sql.gz        — the database dump (pg_dump | gzip)
//   hexeris-files-<stamp>.tar.gz  — the attachments from UPLOAD_DIR (tar+gzip)
// Mode 0600, directory 0700. Both the message bodies in the database and the
// files are already encrypted at rest with SERVER_ENC_KEY. Restoring (see
// docs/operations/BACKUP.md):
//   gunzip -c hexeris-<stamp>.sql.gz | psql "$DATABASE_URL"
//   tar -xzf hexeris-files-<stamp>.tar.gz -C "$UPLOAD_DIR"

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

// backupIntervalHours — the automatic backup interval. Needed both by the
// scheduler and by the health check (to tell whether the last one is overdue).
func backupIntervalHours() int {
	if v, err := strconv.Atoi(os.Getenv("DB_BACKUP_INTERVAL_HOURS")); err == nil && v > 0 {
		return v
	}
	return 24
}

func backupDir() string { return getEnvOrDefault("DB_BACKUP_DIR", "backups") }

// offsiteTimeout — the ceiling on an off-site upload. Without it a hung network
// would hold the scheduler goroutine until the next interval.
const offsiteTimeout = 30 * time.Minute

// backupStatus — the last known backup state for /admin/metrics. Success and
// failure used to live only in the logs: an operator could not answer "when was
// the last backup taken and did it reach off-site storage", and silently
// failing backups were discovered at the moment they were needed.
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

// snapshot — a slice for /admin/metrics.
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
		// A machine code rather than a phrase: the value goes to the UI, and the
		// whole frontend is in US English. The panel composes the text for a
		// human; the server returns the state.
		out["offsite"] = "not_configured"
	}
	return out
}

// startBackupJanitor starts the periodic backup ONLY if it is explicitly enabled.
// Otherwise it quietly does nothing — that is what "available but off" means.
func startBackupJanitor() {
	if !backupEnabled() {
		return
	}
	hours := backupIntervalHours()
	interval := time.Duration(hours) * time.Hour
	log.Printf("DB backup scheduler ENABLED — every %dh into %s", hours, backupDir())

	safeGo("backupJanitor", func() {
		// A catch-up backup at start-up. The loop used to SLEEP the interval and
		// only then take the first dump: a service restarted more often than the
		// interval (deploys, restarts after edits) NEVER made a backup, and that
		// was discovered at the moment one was needed. The age of the freshest
		// file on disk is checked, and if it is older than the interval (or there
		// are no files at all) one is taken immediately.
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

// runBackupOnce takes one dump with pg_dump, compresses it with gzip and prunes
// old ones per DB_BACKUP_KEEP. Returns the path to the created file.
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

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 — the path comes from backupDir
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)

	// pg_dump connects by the DSN itself; the password is passed through the
	// environment rather than arguments so it does not show in the process list.
	cmd := exec.Command("pg_dump", "--no-owner", "--no-privileges", dsn) // #nosec G204 — the dsn comes from the operator's trusted env
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

	// The attachments from UPLOAD_DIR go into a separate archive with the same
	// timestamp so the dump and the files form a consistent backup set. A failure
	// to archive the files does NOT cancel the dump already taken (a partial
	// backup beats none at all).
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

// runOffsiteCopy sends a fresh backup off this machine.
//
// Without it the whole backup sits on the same disk as production: losing the
// VPS (a disk failure, a deleted instance, ransomware) takes both the data and
// its copies at once. That is the main gap in the DR loop, and it is for the
// operator to close — the command is set through DB_BACKUP_OFFSITE_CMD.
//
// A command rather than a hard-wired rsync/rclone/S3: everyone has their own
// destination, and pulling a cloud SDK into the binary for this is unnecessary.
// The path is passed through the ENVIRONMENT (BACKUP_FILE /
// BACKUP_FILES_ARCHIVE / BACKUP_DIR) rather than substituted into a string —
//
// so quoting and spaces raise no questions. An example:
//
//	DB_BACKUP_OFFSITE_CMD='rclone copy "$BACKUP_FILE" remote:hexeris-backups/'
func runOffsiteCopy(dir, dbPath string) {
	cmdline := strings.TrimSpace(os.Getenv("DB_BACKUP_OFFSITE_CMD"))
	if cmdline == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), offsiteTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline) // #nosec G204 — the command comes from the operator's trusted env
	cmd.Env = append(os.Environ(),
		"BACKUP_FILE="+dbPath,
		"BACKUP_FILES_ARCHIVE="+strings.Replace(dbPath, "hexeris-", "hexeris-files-", 1),
		"BACKUP_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// An off-site failure is NOT a reason to call the backup a failure: the
		// local copy is already there. But the fact must be visible, or "we have
		// backups" turns into "we have backups only on the machine that died".
		backupState.recordOffsite(false)
		log.Printf("OFF-SITE backup FAILED: %v — %s", err, strings.TrimSpace(string(out)))
		return
	}
	backupState.recordOffsite(true)
	log.Println("off-site copy ok:", filepath.Base(dbPath))
}

// backupFiles archives the upload directory (UPLOAD_DIR) into
// hexeris-files-<stamp>.tar.gz. An empty or unavailable UPLOAD_DIR is not an
// error (it is simply skipped). The files on disk are already encrypted at
// rest, so the archive is safe to keep next to the dump.
func backupFiles(dir, stamp string) (string, error) {
	src := os.Getenv("UPLOAD_DIR")
	if src == "" {
		return "", nil
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return "", nil // no directory — nothing to archive
	}
	path := filepath.Join(dir, "hexeris-files-"+stamp+".tar.gz")
	tmp := path + ".partial"
	// tar does the gzip itself; "-C src ." stores relative paths (./file) so a
	// restore can go into any directory: tar -xzf FILE -C /new/upload.
	cmd := exec.Command("tar", "-czf", tmp, "-C", src, ".") // #nosec G204 — src comes from the trusted env
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

// pruneOldBackups keeps only the DB_BACKUP_KEEP freshest dumps.
func pruneOldBackups(dir string) {
	keep := 7
	if v, err := strconv.Atoi(os.Getenv("DB_BACKUP_KEEP")); err == nil && v > 0 {
		keep = v
	}
	// Database dumps and file archives are pruned independently, DB_BACKUP_KEEP each.
	for _, pat := range []string{"hexeris-*.sql.gz", "hexeris-files-*.tar.gz"} {
		entries, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil || len(entries) <= keep {
			continue
		}
		sort.Strings(entries) // lexicographic order equals chronological (a UTC timestamp)
		for _, old := range entries[:len(entries)-keep] {
			if err := os.Remove(old); err != nil {
				log.Println("prune backup:", err)
			}
		}
	}
}

// newestBackupAge — the age of the freshest dump in the directory.
// ok=false means "there are no backups at all" (or the directory is unavailable).
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
