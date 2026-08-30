package main

// Exporting an employee's correspondence for a period — CSV or JSON, one file.
//
// ═══ WHY THIS EXISTS ══════════════════════════════════════════════════════
//
// A question asked almost immediately during a pilot: "an employee left in a
// row / there is an internal investigation / a request arrived — export their
// correspondence for a period". Before this endpoint the answer was "go into
// the database with psql", which is no answer: the bodies are encrypted, the
// attachments are separate files with random names, and groups are stored
// differently from direct conversations. The export would have to be assembled
// by hand, at the risk of getting it wrong — and HR and legal decisions rest
// on it.
//
// ═══ HOW THIS ENDPOINT DIFFERS FROM EVERY OTHER ═══════════════════════════
//
// It is the only one that hands DECRYPTED message bodies to the outside.
// Everything else in /admin works with metadata: who, when, from where. Hence
// three restrictions that exist nowhere else:
//
//   1. `user` is mandatory. There is no "export everything" button and there
//      must not be: exporting correspondence is a targeted action about one
//      person, not a dump of the company. One missing parameter must not
//      turn into every conversation of every employee.
//
//   2. `reason` is mandatory and lands in the audit log. A security team asks
//      not "can it be exported" but "who exported it and on what grounds".
//      Without a reason, the log entry "admin exported Ivanov's
//      correspondence" answers nothing.
//
//   3. The export records itself BEFORE the file is served. If the log write
//      fails, the file is not served: an invisible export of someone else's
//      correspondence is worse than one that never happened.
//
// ═══ WHAT GOES IN ═════════════════════════════════════════════════════════
//
// Direct conversations — where the person is sender or recipient. Group
// messages — those in groups they belong to NOW (plus their own messages to
// any group, even one they left: there they are the sender).
//
// An honest boundary: for a group the person left, other people's messages to
// it will not be in the export. Membership history is not kept in the schema —
// there is only joined_at — and pretending the export is complete is not an
// option. It is stated in the export file itself (the notes field in JSON; it
// docs/operations/MESSAGE-EXPORT.md).

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// msgExportMaxRows — the ceiling on one export. The reason is the same as for
// the log export: without it one request would pull years of correspondence
// into memory. Hitting the ceiling is not silent — X-Hexeris-Truncated and the
// truncated field say so.
const msgExportMaxRows = 100000

type msgExportRow struct {
	At          string `json:"at"`  // RFC3339, UTC
	Seq         int64  `json:"seq"` // the write order on the server
	ID          string `json:"id"`
	Chat        string `json:"chat"`      // with whom: the peer or the group name
	ChatKind    string `json:"chat_kind"` // direct | group
	Direction   string `json:"direction"` // sent | received
	Sender      string `json:"sender"`
	Recipient   string `json:"recipient"`
	MediaType   string `json:"media_type,omitempty"`
	Body        string `json:"body"`
	Attachments string `json:"attachments,omitempty"` // file names separated by spaces
	ReplyTo     string `json:"reply_to,omitempty"`
	Forwarded   bool   `json:"forwarded,omitempty"`
	Edited      bool   `json:"edited,omitempty"`
	Deleted     bool   `json:"deleted,omitempty"`
}

func adminMessageExportHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	q := r.URL.Query()

	user := strings.TrimSpace(q.Get("user"))
	if user == "" {
		http.Error(w, "'user' is required: message export is always about one person", http.StatusBadRequest)
		return
	}
	// The name is validated by the same rule as everywhere else: otherwise an
	// arbitrary string from the address would land in the audit log and the
	// export would come out empty with no explanation.
	if !usernameRe.MatchString(user) {
		http.Error(w, "'user' is not a valid user name", http.StatusBadRequest)
		return
	}

	// The reason is no formality: it is the only thing separating a lawful export
	// from curiosity when the log is read six months later.
	reason := strings.TrimSpace(q.Get("reason"))
	if len([]rune(reason)) < 8 {
		http.Error(w, "'reason' is required (at least 8 characters): every export of someone's "+
			"correspondence is recorded together with the reason for it", http.StatusBadRequest)
		return
	}
	if len([]rune(reason)) > 500 {
		reason = string([]rune(reason)[:500])
	}

	from, hasFrom, err := parseDay(q.Get("from"), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	to, hasTo, err := parseDay(q.Get("to"), true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !hasTo {
		to = time.Now()
	}
	if !hasFrom {
		from = to.AddDate(0, 0, -30)
	}
	if to.Before(from) {
		http.Error(w, "'to' is earlier than 'from'", http.StatusBadRequest)
		return
	}

	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		http.Error(w, "format must be csv or json", http.StatusBadRequest)
		return
	}

	// Deleted messages are not exported by default: the person deleted them and
	// they have no place in an ordinary report. In an investigation they are
	// exactly the interesting part, hence a separate flag — and every such row is
	// marked deleted so it cannot be presented as an ordinary one.
	withDeleted := q.Get("deleted") == "1" || strings.EqualFold(q.Get("deleted"), "true")

	// Narrowing to one peer or one group is the typical case: "the correspondence
	// of Ivanov with Petrov", not everything at once.
	peer := strings.TrimSpace(q.Get("with"))

	rows, truncated, err := collectMessageExport(user, peer, from, to, withDeleted)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// The log entry BEFORE the file is served, with its result checked: see point 3
	// in the file header. This is the only place in the project where a failed
	// audit write cancels the operation itself.
	details := fmt.Sprintf("%s..%s %s rows=%d deleted=%v with=%q reason=%s",
		from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"),
		format, len(rows), withDeleted, peer, reason)
	if err := auditErr(r, "message_export", user, details); err != nil {
		http.Error(w, "export refused: the audit record could not be written", http.StatusInternalServerError)
		return
	}

	if truncated {
		w.Header().Set("X-Hexeris-Truncated", strconv.Itoa(msgExportMaxRows))
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	filename := fmt.Sprintf("hexeris-messages-%s-%s.%s", user, stamp, format)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	// Someone else's correspondence must not settle in any cache along the way.
	noStore(w)

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"generated_at":    time.Now().UTC().Format(time.RFC3339),
			"user":            user,
			"with":            peer,
			"from":            from.UTC().Format(time.RFC3339),
			"to":              to.UTC().Format(time.RFC3339),
			"include_deleted": withDeleted,
			"reason":          reason,
			"count":           len(rows),
			"truncated":       truncated,
			"notes":           msgExportNotes,
			"messages":        rows,
		})
		return
	}

	// A UTF-8 BOM so Excel does not read the text in the system code page.
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write([]string{"at_utc", "seq", "chat", "chat_kind", "direction", "sender", "recipient",
		"media_type", "body", "attachments", "reply_to", "forwarded", "edited", "deleted", "message_id"})
	for _, e := range rows {
		cw.Write([]string{e.At, strconv.FormatInt(e.Seq, 10), e.Chat, e.ChatKind, e.Direction,
			e.Sender, e.Recipient, e.MediaType, e.Body, e.Attachments, e.ReplyTo,
			strconv.FormatBool(e.Forwarded), strconv.FormatBool(e.Edited),
			strconv.FormatBool(e.Deleted), e.ID})
	}
	cw.Flush()
}

// msgExportNotes — the limits of the export, stated inside the file itself.
// The report leaves and lives its own life; whoever opens it six months later
// will not be reading the documentation.
var msgExportNotes = []string{
	"Group messages are included for groups the user belongs to at the time of export; " +
		"membership history is not retained, so messages from a group the user has left are " +
		"only included where the user is the sender.",
	"Attachment files are not bundled: the 'attachments' column lists their names as stored " +
		"in UPLOAD_DIR, and the files themselves are encrypted at rest.",
	"Deleted messages are omitted unless explicitly requested, and are flagged when included.",
}

// collectMessageExport assembles the export rows.
func collectMessageExport(user, peer string, from, to time.Time, withDeleted bool) ([]msgExportRow, bool, error) {
	// The group names are pulled in one query rather than per row: an active
	// employee has a handful of groups and tens of thousands of messages.
	groupNames := map[string]string{}
	if grows, err := db.Query(
		`SELECT g.id, g.name FROM groups g JOIN group_members m ON m.group_id = g.id WHERE m.username = $1`,
		user); err == nil {
		defer grows.Close()
		for grows.Next() {
			var id, name string
			if grows.Scan(&id, &name) == nil {
				groupNames[id] = name
			}
		}
	}

	// The coverage condition: direct conversations both ways plus the groups the
	// person belongs to. A subquery on group_members rather than the list from
	// groupNames: that list covers only groups with a row in groups, and a message
	// to a deleted group would be silently left out.
	where := []string{"created_at >= $1", "created_at <= $2",
		"(sender = $3 OR recipient = $3 OR recipient IN (SELECT group_id FROM group_members WHERE username = $3))"}
	args := []any{from, to, user}
	if !withDeleted {
		where = append(where, "deleted = FALSE")
	}
	if peer != "" {
		args = append(args, peer)
		n := len(args)
		// For a group a recipient match is enough; for a person, the pair in
		// either direction.
		if isGroup(peer) {
			where = append(where, fmt.Sprintf("recipient = $%d", n))
		} else {
			where = append(where, fmt.Sprintf("(sender = $%d OR recipient = $%d)", n, n))
		}
	}
	args = append(args, msgExportMaxRows+1)

	query := `SELECT seq, id, sender, recipient, body, COALESCE(media_type,''),
		         COALESCE(reply_to,''), forwarded, COALESCE(edited,false), deleted, created_at
		  FROM messages WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY seq DESC LIMIT $%d", len(args))

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	out := make([]msgExportRow, 0, 1024)
	for rows.Next() {
		var e msgExportRow
		var at time.Time
		var body string
		if err := rows.Scan(&e.Seq, &e.ID, &e.Sender, &e.Recipient, &body, &e.MediaType,
			&e.ReplyTo, &e.Forwarded, &e.Edited, &e.Deleted, &at); err != nil {
			return nil, false, err
		}
		e.At = at.UTC().Format(time.RFC3339)
		// Decryption is the only reason this export cannot be replaced by
		// pg_dump.
		e.Body = decryptBody(body)
		e.Attachments = strings.Join(attachmentNames(e.Body), " ")
		if e.Sender == user {
			e.Direction = "sent"
		} else {
			e.Direction = "received"
		}
		if isGroup(e.Recipient) {
			e.ChatKind = "group"
			if name := groupNames[e.Recipient]; name != "" {
				e.Chat = name + " (" + e.Recipient + ")"
			} else {
				e.Chat = e.Recipient
			}
		} else {
			e.ChatKind = "direct"
			// The peer rather than "the recipient": the column answers "who was
			// the conversation with", which for a sent message is the addressee
			// and for a received one the sender.
			if e.Sender == user {
				e.Chat = e.Recipient
			} else {
				e.Chat = e.Sender
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	// The export is read like a feed: newest on top. seq is the write order on the
	// server, and it is stable when created_at values coincide.
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })

	truncated := false
	if len(out) > msgExportMaxRows {
		out = out[:msgExportMaxRows]
		truncated = true
	}
	return out, truncated, nil
}

// attachmentNames returns the names of the files a message refers to. Without
// them the export is incomplete: the body carries a /files/<name> link, and
// which file on disk that is would have to be answered by hand.
func attachmentNames(body string) []string {
	m := fileRefRe.FindAllStringSubmatch(body, -1)
	if len(m) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(m))
	out := make([]string, 0, len(m))
	for _, g := range m {
		if !seen[g[1]] {
			seen[g[1]] = true
			out = append(out, g[1])
		}
	}
	return out
}
