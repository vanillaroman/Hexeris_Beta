package main

// The attachments of a conversation as a separate list.
//
// ═══ WHY ══════════════════════════════════════════════════════════════════
//
// A screenshot sent three weeks ago was found by scrolling the conversation.
// An attachments panel exists in Telegram, Slack, Mattermost and Rocket.Chat —
// it is not a distinguishing feature but a standard that was missing.
//
// ═══ WHY THIS IS CHEAP AND A NAME SEARCH IS NOT ══════════════════════════
//
// media_type is a PLAINTEXT column in messages, so selecting by type is an
// ordinary indexed query. The file name lives inside body, and body is
// encrypted with AES-256-GCM: it cannot be searched without decrypting
// everything, exactly as searchHandler does (up to 20,000 messages per query).
//
// Hence the boundary: the server returns a PAGE of attachments and decrypts
// only that — 60 rows instead of the whole history. The name filter works on
// the client over the page already received. That deliberately leaves the
// filter incomplete on very long histories; a full pass would cost as much as
// the search, and adding one must be a separate decision, not a side effect.
//
// ═══ THE ACCESS BOUNDARY ═════════════════════════════════════════════════
//
// The same as for history, and checked here AGAIN rather than inherited: group
// membership for a group, participation in the pair for a direct conversation.
// An endpoint that hands out file links must not rely on the client asking
// only for its own.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// attachmentKinds — what each panel tab shows.
//
// "Media" deliberately merges photos and video: a person looks for "the
// picture that was sent" without separating them by codec. Calls
// (media_type="call") do not land here — that is an event, not an attachment,
// and in a list of files it would be noise.
var attachmentKinds = map[string][]string{
	"media": {"image", "video"},
	"files": {"document"},
	"voice": {"voice"},
}

type attachmentItem struct {
	Seq       int64  `json:"seq"`
	ID        string `json:"id"`
	From      string `json:"from"`
	URL       string `json:"url"`
	MediaType string `json:"media_type"`
	CreatedAt int64  `json:"created_at"`
}

func attachmentsHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The limiter is shared with history: both endpoints hit the same table with
	// the same weight, and separate budgets would let one bypass the other.
	if historyLimiter.isBlocked(username) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	historyLimiter.recordFailure(username)
	ctx := r.Context()

	peer := r.URL.Query().Get("peer")
	if peer == "" {
		http.Error(w, "peer required", http.StatusBadRequest)
		return
	}
	kinds, okKind := attachmentKinds[r.URL.Query().Get("kind")]
	if !okKind {
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	if isGroup(peer) && !isGroupMember(peer, username) {
		http.Error(w, "not a member", http.StatusForbidden)
		return
	}

	limit := 60
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
		if limit <= 0 || limit > 200 {
			limit = 60
		}
	}
	// A cursor into the past. Zero means "from the freshest"; a deliberately
	// larger seq goes into the query so there is no second SQL variant.
	var before int64 = 1<<62 - 1
	if b := r.URL.Query().Get("before"); b != "" {
		fmt.Sscanf(b, "%d", &before)
		if before <= 0 {
			before = 1<<62 - 1
		}
	}

	// The type list is passed as parameters rather than concatenated into the
	// string: the values come from the map above and concatenation would be safe,
	// but the rule "SQL is not built by concatenation" admits no exceptions.
	ph := make([]string, len(kinds))
	args := []any{}
	if isGroup(peer) {
		args = append(args, peer, before)
	} else {
		args = append(args, username, peer, before)
	}
	for i, k := range kinds {
		ph[i] = fmt.Sprintf("$%d", len(args)+1)
		args = append(args, k)
	}
	args = append(args, limit)
	limitPh := fmt.Sprintf("$%d", len(args))

	var q string
	if isGroup(peer) {
		q = `SELECT seq, id, sender, body, COALESCE(media_type,''),
		            EXTRACT(EPOCH FROM created_at)*1000
		     FROM messages
		     WHERE recipient=$1 AND seq < $2 AND deleted = false
		       AND media_type IN (` + strings.Join(ph, ",") + `)
		     ORDER BY seq DESC LIMIT ` + limitPh
	} else {
		q = `SELECT seq, id, sender, body, COALESCE(media_type,''),
		            EXTRACT(EPOCH FROM created_at)*1000
		     FROM messages
		     WHERE ((sender=$1 AND recipient=$2) OR (sender=$2 AND recipient=$1))
		       AND seq < $3 AND deleted = false
		       AND media_type IN (` + strings.Join(ph, ",") + `)
		     ORDER BY seq DESC LIMIT ` + limitPh
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		log.Printf("attachments query: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	items := []attachmentItem{}
	var last int64
	for rows.Next() {
		var it attachmentItem
		var body string
		var ts float64
		if err := rows.Scan(&it.Seq, &it.ID, &it.From, &body, &it.MediaType, &ts); err != nil {
			continue
		}
		// ONLY this page is decrypted — that is the whole saving.
		it.URL = decryptBody(body)
		it.CreatedAt = int64(ts)
		items = append(items, it)
		last = it.Seq
	}

	// next is returned only when the page filled up completely: otherwise the
	// client would make one more certainly empty request per conversation.
	next := int64(0)
	if len(items) == limit {
		next = last
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"items": items, "next": next})
}
