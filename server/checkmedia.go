package main

// The `./hexeris check-media` subcommand — reconciling attachments with the disk.
//
// ═══ WHY ══════════════════════════════════════════════════════════════════
//
// The link to an attachment lives in the message body (usually encrypted)
// while the file itself lives on disk. The two halves drift apart silently: a
// move to another server, a database restored from a copy fresher than the
// upload directory, a manual disk cleanup. From outside the divergence looks
// like "some pictures in the conversation do not open", and that cannot be
// investigated one picture at a time in a browser.
//
// The command walks the messages, extracts the links and says which files are
// missing. Read-only: it changes and deletes nothing.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// fileRefRe catches links of the form /files/<name> in a message body. The
// name is limited to the alphabet randomFileName creates them with.
var fileRefRe = regexp.MustCompile(`/files/([A-Za-z0-9_.-]+)`)

func runCheckMedia(args []string) {
	verbose := false
	for _, a := range args {
		if a == "-v" || a == "--verbose" {
			verbose = true
		}
	}

	// Exactly the three variables that are needed are checked, and the missing
	// ones are all named at once: an administrator should not discover them one
	// by one, getting a new error after every edit.
	var missingEnv []string
	for _, name := range []string{"DATABASE_URL", "SERVER_ENC_KEY", "UPLOAD_DIR"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missingEnv = append(missingEnv, name)
		}
	}
	if len(missingEnv) > 0 {
		fmt.Fprintf(os.Stderr, "check-media: not set: %s\n\n", strings.Join(missingEnv, ", "))
		fmt.Fprintln(os.Stderr, "The values are the ones the service runs with. They can be taken like this:")
		fmt.Fprintln(os.Stderr, "  systemctl show hexeris -p Environment")
		fmt.Fprintln(os.Stderr, "  cat /etc/systemd/system/hexeris.service.d/*.conf")
		fmt.Fprintln(os.Stderr, "\nOr in one line, picking up the environment of the running process:")
		fmt.Fprintln(os.Stderr, "  sudo tr '\\0' '\\n' < /proc/$(pidof hexeris)/environ > /tmp/hexeris.env")
		fmt.Fprintln(os.Stderr, "  set -a; . /tmp/hexeris.env; set +a; ./hexeris check-media; rm -f /tmp/hexeris.env")
		os.Exit(1)
	}

	// The key is needed: message bodies are encrypted, and without decrypting them
	// the links are invisible. The directory is the one the running server uses.
	encKey()
	dir := uploadDir()

	// The connection is made here: the server-side initDB brings up the schema,
	// migrations and background jobs, while this check only needs to read.
	initDB()

	rows, err := db.Query(`
		SELECT id, sender, recipient, created_at, COALESCE(media_type,''), body
		FROM messages
		WHERE deleted = false AND body LIKE '%%' ORDER BY seq`)
	if err != nil {
		log.Fatalf("could not read the messages: %v", err)
	}
	defer rows.Close()

	type ref struct {
		msgID, sender, recipient, when, mediaType string
	}
	// One file may be forwarded many times — the first mention and a counter are
	// kept, otherwise the report turns into a wall of text.
	missing := map[string]*ref{}
	missingCount := map[string]int{}
	var total, present int

	for rows.Next() {
		var id, sender, recipient, when, mtype, body string
		if err := rows.Scan(&id, &sender, &recipient, &when, &mtype, &body); err != nil {
			continue
		}
		plain := decryptBody(body)
		for _, m := range fileRefRe.FindAllStringSubmatch(plain, -1) {
			name := m[1]
			// filepath.Base — the same normalisation as in filesHandler: exactly
			// what the server would open is what gets checked.
			name = filepath.Base(name)
			total++
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				present++
				continue
			}
			missingCount[name]++
			if missing[name] == nil {
				missing[name] = &ref{msgID: id, sender: sender, recipient: recipient, when: when, mediaType: mtype}
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("the walk over messages was interrupted: %v", err)
	}

	fmt.Printf("upload directory: %s\n", dir)
	fmt.Printf("attachment links in messages: %d\n", total)
	fmt.Printf("files present: %d\n", present)
	fmt.Printf("files NOT found: %d (unique names: %d)\n", total-present, len(missing))

	if len(missing) == 0 {
		fmt.Println("\nEvery attachment is present.")
		return
	}

	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)

	limit := 20
	if verbose {
		limit = len(names)
	}
	fmt.Println("\nMissing:")
	for i, n := range names {
		if i >= limit {
			fmt.Printf("  … and %d more (run with -v to see them all)\n", len(names)-limit)
			break
		}
		r := missing[n]
		fmt.Printf("  %-40s  first mention: %s -> %s, %s", n, r.sender, r.recipient, r.when)
		if missingCount[n] > 1 {
			fmt.Printf("  (mentions: %d)", missingCount[n])
		}
		fmt.Println()
	}

	// The dates of first mentions are the most useful hint: if every loss lies
	// before a certain day, what was lost is not a single file but a whole period,
	// and the thing to look for is a move or a restore from a copy.
	oldest, newest := "", ""
	for _, n := range names {
		w := missing[n].when
		if oldest == "" || w < oldest {
			oldest = w
		}
		if newest == "" || w > newest {
			newest = w
		}
	}
	fmt.Printf("\nThe missing attachments fall in the period from %s to %s.\n",
		strings.TrimSpace(oldest), strings.TrimSpace(newest))
	fmt.Println("If the period is continuous, what was lost is not one file but the directory for that time:")
	fmt.Println("check the move to another server and that the database copy matches the upload directory copy.")
}
