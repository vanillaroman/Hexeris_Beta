package main

// History search: a bounded scan with decryption on the fly.

import (
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"path"
	"strings"
)

// fileSearchName returns the searchable file name: the original one from the
// #fragment (/files/<random>.<ext>#<url-encoded name>), or the on-disk name
// for older uploads that have no fragment.
func fileSearchName(body string) string {
	name := ""
	if i := strings.LastIndex(body, "#"); i >= 0 {
		if dec, err := neturl.PathUnescape(body[i+1:]); err == nil {
			name = dec
		} else {
			name = body[i+1:]
		}
		body = body[:i]
	}
	if name == "" {
		name = path.Base(body)
	}
	return name
}

// Bodies are encrypted at rest, so SQL LIKE and full-text indexes would need
// a plaintext index that defeats the encryption. Instead the user's own
// messages are scanned and decrypted on the fly (GCM costs microseconds per
// message), bounded by maxScan rows per request, with a keyset cursor
// (before=<seq>) so the client can continue deeper on demand.

func makeSnippet(body, qLower string) string {
	const ctx = 60 // characters of context on each side
	runes := []rune(body)
	if len(runes) <= 2*ctx {
		return body
	}
	i := strings.Index(strings.ToLower(body), qLower)
	if i < 0 {
		return string(runes[:2*ctx]) + "…"
	}
	// Convert the byte index to runes so UTF-8 is not cut mid-character.
	pos := len([]rune(body[:i]))
	start, end := pos-ctx, pos+ctx
	pre, post := "…", "…"
	if start <= 0 {
		start, pre = 0, ""
	}
	if end >= len(runes) {
		end, post = len(runes), ""
	}
	return pre + string(runes[start:end]) + post
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if searchLimiter.isBlocked(username) {
		http.Error(w, "too many searches, try again later", http.StatusTooManyRequests)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	qLower := strings.ToLower(q)
	if len([]rune(q)) < 2 {
		http.Error(w, "query too short", http.StatusBadRequest)
		return
	}
	searchLimiter.recordFailure(username)
	var cursor int64 = 1<<62 - 1
	fmt.Sscanf(r.URL.Query().Get("before"), "%d", &cursor)

	const scanBatch, maxScan, maxHits = 2000, 20000, 30

	type Hit struct {
		ID        string  `json:"id"`
		Seq       int64   `json:"seq"`
		Peer      string  `json:"peer"`
		From      string  `json:"from"`
		Snippet   string  `json:"snippet"`
		CreatedAt float64 `json:"created_at"`
	}
	hits := []Hit{}
	scanned := 0
	exhausted := false

	for scanned < maxScan && len(hits) < maxHits && !exhausted {
		// Deleted rows are filtered in SQL rather than in Go: otherwise they
		// consume slots in the 2000-row batch and the scan needs more
		// iterations to reach the same number of real text messages.
		rows, err := db.QueryContext(r.Context(), `
			SELECT seq, id, sender, recipient, body, COALESCE(media_type,''),
			       EXTRACT(EPOCH FROM created_at)*1000
			FROM messages
			WHERE (sender=$1 OR recipient=$1
			   OR recipient IN (SELECT group_id FROM group_members WHERE username=$1))
			  AND seq < $2
			  AND deleted = false
			ORDER BY seq DESC LIMIT $3`, username, cursor, scanBatch)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		got := 0
		for rows.Next() {
			var seq int64
			var id, sender, recipient, body, mt string
			var createdAt float64
			if rows.Scan(&seq, &id, &sender, &recipient, &body, &mt, &createdAt) != nil {
				continue
			}
			got++
			scanned++
			cursor = seq
			// Text matches against the decrypted body, files against their
			// name. Other media (call logs, voice notes) are not searchable.
			var snippet string
			if mt == "" {
				body = decryptBody(body)
				if !strings.Contains(strings.ToLower(body), qLower) {
					continue
				}
				snippet = makeSnippet(body, qLower)
			} else if strings.HasPrefix(body, "/files/") {
				name := fileSearchName(body)
				if !strings.Contains(strings.ToLower(name), qLower) {
					continue
				}
				snippet = name
			} else {
				continue
			}
			peer := recipient
			if isGroup(recipient) {
				// a hit in a group points at the group itself
			} else if sender != username {
				peer = sender
			}
			hits = append(hits, Hit{ID: id, Seq: seq, Peer: peer, From: sender,
				Snippet: snippet, CreatedAt: createdAt})
			if len(hits) >= maxHits {
				break
			}
		}
		rows.Close()
		if got < scanBatch {
			exhausted = true
		}
	}

	next := cursor
	if exhausted {
		next = 0 // nothing further to scan
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"hits": hits, "next": next, "scanned": scanned})
}
