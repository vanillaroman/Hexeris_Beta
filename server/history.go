package main

// Message history, reaction sync and offline delivery.
// Part of the Hexeris server; the shared types and main() live in main.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/lib/pq"
)

// ─── /history — incremental, per-conversation, ordered by seq ─────────────────

func reactionsHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if reactionsSyncLimiter.isBlocked(username) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	reactionsSyncLimiter.recordFailure(username)
	var since int64
	fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)

	rows, err := db.QueryContext(r.Context(), `
		SELECT r.rseq, r.msg_id, r.username, r.emoji, r.removed, m.sender, m.recipient
		FROM reactions r JOIN messages m ON m.id = r.msg_id
		WHERE r.rseq > $2 AND (m.sender = $1 OR m.recipient = $1
		      OR m.recipient IN (SELECT group_id FROM group_members WHERE username = $1))
		ORDER BY r.rseq ASC LIMIT 500`, username, since)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type REvent struct {
		RSeq    int64  `json:"rseq"`
		MsgID   string `json:"msg_id"`
		From    string `json:"from"`
		Emoji   string `json:"emoji"`
		Removed bool   `json:"removed"`
		Sender  string `json:"sender"`
		To      string `json:"recipient"`
	}
	events := []REvent{}
	for rows.Next() {
		var e REvent
		if rows.Scan(&e.RSeq, &e.MsgID, &e.From, &e.Emoji, &e.Removed, &e.Sender, &e.To) == nil {
			events = append(events, e)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func historyHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if historyLimiter.isBlocked(username) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	historyLimiter.recordFailure(username)
	// Queries live no longer than the HTTP request: if the client closes the
	// connection (left the page, lost the network) Postgres cancels the query
	// instead of computing it into the void.
	ctx := r.Context()

	peer := r.URL.Query().Get("peer")
	sinceStr := r.URL.Query().Get("since")
	var since int64
	if sinceStr != "" {
		fmt.Sscanf(sinceStr, "%d", &since)
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
		if limit <= 0 || limit > 1000 {
			limit = 200
		}
	}

	// before=<seq> — paging UPWARDS (into the past): the client scrolled to the
	// start of the loaded chunk and asks for an older page. Requires peer: paging
	// "everything at once" backwards is pointless, scrolling is always in one chat.
	var before int64
	if b := r.URL.Query().Get("before"); b != "" && peer != "" {
		fmt.Sscanf(b, "%d", &before)
	}

	if isGroup(peer) && !isGroupMember(peer, username) {
		http.Error(w, "not a member", http.StatusForbidden)
		return
	}
	var rows *sql.Rows
	var err error
	if before > 0 {
		// Take a page below the cursor in descending order (so LIMIT cuts off the
		// ones nearest the cursor, not the most ancient); the client sorts them.
		if isGroup(peer) {
			rows, err = db.QueryContext(ctx, `
				SELECT seq, id, sender, recipient, body, COALESCE(media_type,''),
				       COALESCE(reply_to,''), forwarded, delivered, read, deleted, COALESCE(edited,false),
				       EXTRACT(EPOCH FROM created_at)*1000
				FROM messages
				WHERE recipient=$1 AND seq < $2
				ORDER BY seq DESC
				LIMIT $3`, peer, before, limit)
		} else {
			rows, err = db.QueryContext(ctx, `
				SELECT seq, id, sender, recipient, body, COALESCE(media_type,''),
				       COALESCE(reply_to,''), forwarded, delivered, read, deleted, COALESCE(edited,false),
				       EXTRACT(EPOCH FROM created_at)*1000
				FROM messages
				WHERE ((sender=$1 AND recipient=$2) OR (sender=$2 AND recipient=$1))
				  AND seq < $3
				ORDER BY seq DESC
				LIMIT $4`, username, peer, before, limit)
		}
	} else if isGroup(peer) {
		// Membership is checked above; username is not needed in the query — the
		// number of placeholders must match the number of arguments (or it is a 500).
		rows, err = db.QueryContext(ctx, `
			SELECT seq, id, sender, recipient, body, COALESCE(media_type,''),
			       COALESCE(reply_to,''), forwarded, delivered, read, deleted, COALESCE(edited,false),
			       EXTRACT(EPOCH FROM created_at)*1000
			FROM messages
			WHERE recipient=$1 AND seq > $2
			ORDER BY seq ASC
			LIMIT $3`, peer, since, limit)
	} else if peer != "" {
		// One conversation, ascending, incremental.
		rows, err = db.QueryContext(ctx, `
			SELECT seq, id, sender, recipient, body, COALESCE(media_type,''),
			       COALESCE(reply_to,''), forwarded, delivered, read, deleted, COALESCE(edited,false),
			       EXTRACT(EPOCH FROM created_at)*1000
			FROM messages
			WHERE ((sender=$1 AND recipient=$2) OR (sender=$2 AND recipient=$1))
			  AND seq > $3
			ORDER BY seq ASC
			LIMIT $4`, username, peer, since, limit)
	} else {
		// All conversations (used on first load / reconnect), ascending.
		//
		// The shape of the query matters more here than the indexes. A single
		// OR condition (sender=$1 OR recipient=$1 OR recipient IN (…)) was executed
		// by Postgres as a scan of messages_pkey with row-by-row filtering: to
		// return a page it walked the table from the cursor onwards, discarding
		// rows that belonged to others. The cost was set by the size of the WHOLE
		// table rather than of the answer. At 300k messages, a reconnect by a user
		// with no new messages discarded 300,426 rows in 110 ms — and grew linearly
		// with the database. That, not body decryption, is what made /history the
		// heaviest endpoint under concurrency.
		//
		// A UNION ALL of three independent branches, each with its own LIMIT,
		// gives range scans over (sender,seq) and (recipient,seq) that stop once
		// they have collected enough rows. The same worst case is 0.14 ms
		// (measurements in docs/engineering/ARCHITECTURE.md §6).
		//
		// The group list is resolved by a separate query rather than a subquery:
		// with IN (SELECT …) the planner built a nested loop over the whole
		// messages table (116 ms even when the user has no groups). An empty list
		// simply removes the third branch.
		groups, gerr := userGroupIDs(ctx, username)
		if gerr != nil {
			log.Println("history: group list failed:", gerr)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		// The branches select only seq; the bodies are fetched in one pass over
		// the primary key. IN also removes the duplicate of a message to oneself,
		// which lands in both the sender and the recipient branch.
		branches := `
			  (SELECT seq FROM messages WHERE sender=$1    AND seq > $2 ORDER BY seq ASC LIMIT $3)
			  UNION ALL
			  (SELECT seq FROM messages WHERE recipient=$1 AND seq > $2 ORDER BY seq ASC LIMIT $3)`
		args := []any{username, since, limit}
		if len(groups) > 0 {
			branches += `
			  UNION ALL
			  (SELECT seq FROM messages WHERE recipient = ANY($4) AND seq > $2 ORDER BY seq ASC LIMIT $3)`
			args = append(args, pq.Array(groups))
		}
		rows, err = db.QueryContext(ctx, `
			SELECT seq, id, sender, recipient, body, COALESCE(media_type,''),
			       COALESCE(reply_to,''), forwarded, delivered, read, deleted, COALESCE(edited,false),
			       EXTRACT(EPOCH FROM created_at)*1000
			FROM messages
			WHERE seq IN (`+branches+`)
			ORDER BY seq ASC
			LIMIT $3`, args...)
	}
	if err != nil {
		log.Println("history query failed:", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type HistMsg struct {
		Seq       int64  `json:"seq"`
		ID        string `json:"id"`
		From      string `json:"from"`
		To        string `json:"to"`
		Body      string `json:"body"`
		MediaType string `json:"media_type"`
		ReplyTo   string `json:"reply_to"`
		Forwarded bool   `json:"forwarded"`
		Delivered bool   `json:"delivered"`
		Read      bool   `json:"read"`
		Deleted   bool   `json:"deleted"`
		Edited    bool   `json:"edited"`
		CreatedAt int64  `json:"created_at"`
		// Always present in the JSON (no omitempty): the client overwrites its
		// local state with the server one, removed reactions included.
		Reactions map[string][]string `json:"reactions"`
	}
	// The personal visibility boundary after "clear the conversation" (see
	// chatprefs.go). Filtering happens in Go rather than as a SQL condition,
	// deliberately: the "all conversations" branch is built for specific range
	// scans (the comment above), and an extra condition risks returning to a
	// full-table scan for a feature few people use. The map is nearly always
	// empty, and then the check costs one length comparison.
	cleared := clearedSeqs(username)

	msgs := []HistMsg{}
	for rows.Next() {
		var m HistMsg
		var createdAt float64
		if err := rows.Scan(&m.Seq, &m.ID, &m.From, &m.To, &m.Body, &m.MediaType,
			&m.ReplyTo, &m.Forwarded, &m.Delivered, &m.Read, &m.Deleted, &m.Edited, &createdAt); err != nil {
			continue
		}
		if len(cleared) > 0 && m.Seq <= cleared[chatKeyFor(username, m.From, m.To)] {
			continue
		}
		m.CreatedAt = int64(createdAt)
		if m.Deleted {
			m.Body = "[deleted]"
		} else if m.MediaType == "" {
			m.Body = decryptBody(m.Body)
		}
		msgs = append(msgs, m)
	}

	// Reactions for the whole page in one query (not N+1).
	if len(msgs) > 0 {
		byID := make(map[string]int, len(msgs))
		ids := make([]string, len(msgs))
		for i := range msgs {
			msgs[i].Reactions = map[string][]string{}
			byID[msgs[i].ID] = i
			ids[i] = msgs[i].ID
		}
		if rrows, rerr := db.QueryContext(ctx, `SELECT msg_id, username, emoji FROM reactions
			WHERE msg_id = ANY($1) AND removed = FALSE ORDER BY created_at`, pq.Array(ids)); rerr == nil {
			for rrows.Next() {
				var mid, user, emoji string
				if rrows.Scan(&mid, &user, &emoji) != nil {
					continue
				}
				if i, ok := byID[mid]; ok {
					msgs[i].Reactions[user] = append(msgs[i].Reactions[user], emoji)
				}
			}
			rrows.Close()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

// ─── Offline delivery ─────────────────────────────────────────────────────────

// userGroupIDs — the user's group identifiers. A separate query rather than a
// subquery inside the history select: as a subquery it made the planner build
// a nested loop over the whole messages table (see the comment in
// historyHandler).
func userGroupIDs(ctx context.Context, username string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT group_id FROM group_members WHERE username=$1`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// maxPendingPerConnect is the ceiling on the offline queue drained in a single
// connection. The query used to have no LIMIT: a user with a large backlog (or
// the recipient of a spam run) pulled EVERY undelivered row into memory at once
// and decrypted each one — on a mass reconnect that multiplied by the number of
// clients and became an OOM vector.
//
// The ceiling loses nothing: what does not fit stays delivered=false and arrives
// in the next batch. The client picks up the tail with the ordinary seq sync
// (/history?since=…), which has already run by that point. The value is above
// the depth of a client's outgoing queue (clientSendBuffer=256): hitting that
// first is a normal scenario and also leaves a remainder in pending.
//
// A variable rather than a constant purely for the test (see delivery_test.go):
// the value does not change in production.
var maxPendingPerConnect = 1000

func deliverPending(username string, client *Client) {
	rows, err := db.Query(`
		SELECT seq, id, sender, body, COALESCE(media_type,''),
		       COALESCE(reply_to,''), forwarded,
		       EXTRACT(EPOCH FROM created_at)*1000
		FROM messages
		WHERE recipient=$1 AND delivered=false
		ORDER BY seq ASC
		LIMIT $2`, username, maxPendingPerConnect)
	if err != nil {
		return
	}
	type pend struct {
		seq                int64
		id, sender, body   string
		mediaType, replyTo string
		forwarded          bool
		createdAt          float64
	}
	// The same visibility boundary as in the history: a conversation cleared on
	// another device must not resurrect from the offline queue. The skipped rows
	// still go into the shared UPDATE below — otherwise they would hang
	// undelivered forever and arrive on every connection.
	cleared := clearedSeqs(username)

	var pending []pend
	for rows.Next() {
		var p pend
		if rows.Scan(&p.seq, &p.id, &p.sender, &p.body, &p.mediaType,
			&p.replyTo, &p.forwarded, &p.createdAt) == nil {
			pending = append(pending, p)
		}
	}
	rows.Close()

	// Delivered rows are marked with ONE UPDATE at the end rather than a query
	// per message: with a large offline backlog that was an N+1 which drained
	// the connection pool on a mass reconnect (a load test). The order of
	// guarantees does not change: a row is marked delivered only after a
	// successful write to the socket, and an interruption halfway leaves the
	// remainder pending until the next reconnect.
	var deliveredIDs []string
	for _, p := range pending {
		// The queue is personal (recipient=$1), so the conversation key is the
		// sender. Group messages do not land here: they are marked delivered at once.
		if len(cleared) > 0 && p.seq <= cleared[p.sender] {
			// Marked delivered anyway: the message reached the recipient, it was
			// their own setting that hid it. Otherwise the row would stay pending
			// forever and the sender would always see "not delivered".
			deliveredIDs = append(deliveredIDs, p.id)
			sendACKToAll(p.sender, p.id, "delivered", p.seq)
			continue
		}
		body := p.body
		if p.mediaType == "" {
			body = decryptBody(body)
		}
		msg := Message{
			Type: "message", ID: p.id, Seq: p.seq, From: p.sender, To: username,
			Body: body, MediaType: p.mediaType,
			ReplyTo: p.replyTo, Forwarded: p.forwarded, CreatedAt: int64(p.createdAt),
		}
		data, _ := json.Marshal(msg)
		if err := client.send(data); err != nil {
			// Socket write failed — stop; remaining stay pending for next reconnect.
			break
		}
		deliveredIDs = append(deliveredIDs, p.id)
		sendACKToAll(p.sender, p.id, "delivered", p.seq)
	}
	if len(deliveredIDs) > 0 {
		db.Exec("UPDATE messages SET delivered=true WHERE id = ANY($1)", pq.Array(deliveredIDs))
	}
}
