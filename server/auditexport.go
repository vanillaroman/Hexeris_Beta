package main

// Exporting the audit logs for a period — CSV or JSON, one file.
//
// Why a separate endpoint when /admin/audit and /admin/login-audit exist: those
// are made for a SCREEN — they return pages of 100–500 records with an offset,
// and a quarter cannot be assembled from them by hand. The customer's security
// team asks for exactly one thing: "export the log for a period and send it as
// a file". Without that, a pilot conversation runs into "we will look in the
// panel", which is no use for an internal investigation or an audit.
//
// The two logs are deliberately different and are joined here only by kind:
//   admin — administrator actions (who was created, blocked, deleted);
//   login — sign-ins and failed attempts (who, from where, by what method).
// The first answers "what was done to the system", the second "who entered
// it". A security team almost always asks for both.
//
// The period bounds cover whole days: from=2026-08-01&to=2026-08-31 means
// 00:00 on the first through 23:59:59 on the thirty-first. Otherwise the last
// day would be lost systematically, noticed only from an incomplete export.

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

// exportMaxRows — the export ceiling. Not for thrift: without it one request
// could pull years of log into memory and kill the process, and the admin
// panel is reachable from outside. On reaching the ceiling we say so honestly
// with an X-Hexeris-Truncated header rather than silently serving a stub.
const exportMaxRows = 200000

// parseDay parses YYYY-MM-DD or a full RFC3339. An empty string is not an
// error: it means "no bound", and the caller substitutes its own.
func parseDay(s string, endOfDay bool) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("bad date %q: use YYYY-MM-DD or RFC3339", s)
	}
	if endOfDay {
		t = t.Add(24*time.Hour - time.Nanosecond)
	}
	return t, true, nil
}

type auditExportRow struct {
	At      string `json:"at"`                // RFC3339, UTC
	Kind    string `json:"kind"`              // admin | login
	Actor   string `json:"actor"`             // who (for login, the username)
	Action  string `json:"action"`            // what they did / the sign-in outcome
	Target  string `json:"target,omitempty"`  // on whom or what
	Details string `json:"details,omitempty"` // free text
	IP      string `json:"ip,omitempty"`
	Method  string `json:"method,omitempty"` // login only
	UA      string `json:"user_agent,omitempty"`
}

func adminAuditExportHandler(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}

	from, hasFrom, err := parseDay(r.URL.Query().Get("from"), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	to, hasTo, err := parseDay(r.URL.Query().Get("to"), true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The defaults are chosen so a request with no parameters is meaningful
	// rather than empty: the last 30 days is the typical reporting period.
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

	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind == "" {
		kind = "all"
	}
	if kind != "all" && kind != "admin" && kind != "login" {
		http.Error(w, "kind must be all, admin or login", http.StatusBadRequest)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		http.Error(w, "format must be csv or json", http.StatusBadRequest)
		return
	}

	rows, truncated, err := collectAuditRows(kind, from, to)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// The export is itself an administrator action and must be in the log:
	// "who took the log outside" is the first question when a leak is examined.
	audit(r, "audit_export", kind,
		fmt.Sprintf("%s..%s %s rows=%d", from.UTC().Format("2006-01-02"),
			to.UTC().Format("2006-01-02"), format, len(rows)))

	if truncated {
		w.Header().Set("X-Hexeris-Truncated", strconv.Itoa(exportMaxRows))
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	filename := fmt.Sprintf("hexeris-audit-%s-%s.%s", kind, stamp, format)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"generated_at": time.Now().UTC().Format(time.RFC3339),
			"from":         from.UTC().Format(time.RFC3339),
			"to":           to.UTC().Format(time.RFC3339),
			"kind":         kind,
			"count":        len(rows),
			"truncated":    truncated,
			"entries":      rows,
		})
		return
	}

	// A UTF-8 BOM: without it Excel opens the CSV in the system code page and
	// non-ASCII text turns to rubbish. A customer opens the export in Excel,
	// not in a text editor.
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write([]string{"at_utc", "kind", "actor", "action", "target", "details", "ip", "method", "user_agent"})
	for _, e := range rows {
		cw.Write([]string{e.At, e.Kind, e.Actor, e.Action, e.Target, e.Details, e.IP, e.Method, e.UA})
	}
	cw.Flush()
}

// collectAuditRows reads both logs and merges them by time, newest first.
func collectAuditRows(kind string, from, to time.Time) ([]auditExportRow, bool, error) {
	out := make([]auditExportRow, 0, 1024)

	if kind == "all" || kind == "admin" {
		rows, err := db.Query(`
			SELECT action, COALESCE(target,''), COALESCE(details,''), COALESCE(ip,''), created_at
			FROM admin_audit
			WHERE created_at >= $1 AND created_at <= $2
			ORDER BY created_at DESC LIMIT $3`, from, to, exportMaxRows+1)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		for rows.Next() {
			var e auditExportRow
			var at time.Time
			if err := rows.Scan(&e.Action, &e.Target, &e.Details, &e.IP, &at); err != nil {
				return nil, false, err
			}
			e.At = at.UTC().Format(time.RFC3339)
			e.Kind = "admin"
			// admin_audit has one actor — whoever holds the admin key. There are
			// no separate administrator accounts (see SECURITY.md), and pretending
			// otherwise in an export is not acceptable: decisions rest on it. Who
			// exactly holds the key is an organisational question.
			e.Actor = "admin"
			out = append(out, e)
		}
	}

	if kind == "all" || kind == "login" {
		rows, err := db.Query(`
			SELECT username, outcome, method, COALESCE(ip,''), COALESCE(user_agent,''), created_at
			FROM login_audit
			WHERE created_at >= $1 AND created_at <= $2
			ORDER BY created_at DESC LIMIT $3`, from, to, exportMaxRows+1)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		for rows.Next() {
			var e auditExportRow
			var at time.Time
			if err := rows.Scan(&e.Actor, &e.Action, &e.Method, &e.IP, &e.UA, &at); err != nil {
				return nil, false, err
			}
			e.At = at.UTC().Format(time.RFC3339)
			e.Kind = "login"
			out = append(out, e)
		}
	}

	// Sorted by descending time: the two logs arrive separately while the export
	// will be read as a single feed of events.
	//
	// sort.Slice rather than insertion: the export ceiling is 200,000 rows, and a
	// quadratic sort means tens of billions of comparisons — a request that hangs
	// for minutes instead of a file. The comparison is by string and that is
	// correct: RFC3339 in UTC with fixed-width fields orders lexicographically
	// exactly as it orders chronologically.
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })

	truncated := false
	if len(out) > exportMaxRows {
		out = out[:exportMaxRows]
		truncated = true
	}
	return out, truncated, nil
}
