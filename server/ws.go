package main

// WebSocket: accepting connections, routing messages, ACKs and persistence.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
	"unicode/utf8"
)

// ─── WebSocket ─────────────────────────────────────────────────────────────────

// maxMessageRunes limits text length in runes, not bytes, and applies to
// both WS sends and /edit-message so neither is a bypass of the other.
const maxMessageRunes = 4096

// Per-message delivery logging, off by default; see the use site.
var debugMessageLog = os.Getenv("DEBUG_MESSAGE_LOG") == "1"

func wsHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// A token may predate the block, so this is checked on every connect.
	if userBlocked(username) {
		http.Error(w, "account is blocked", http.StatusForbidden)
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// Frame size ceiling. An SDP offer with ICE candidates is a few KB and
	// text is limited separately below, so 64 KB is generous; without a
	// limit one client can send a frame of hundreds of MB.
	conn.SetReadLimit(64 << 10)
	// newClient starts the writer before deliverPending below, or the first
	// frames would queue with nobody draining them.
	client := newClient(username, conn)

	mu.Lock()
	clients[username] = append(clients[username], client)
	nDevices := len(clients[username]) // read under the lock: a concurrent
	mu.Unlock()                        // connect rewrites this slice
	statWSConnects.Add(1)
	log.Println(username, "connected (", nDevices, "devices)")

	// Cancel the deferred "offline" broadcast: a page reload reconnects
	// within seconds, and nobody else should see an offline→online flicker.
	if t, ok := pendingOffline.LoadAndDelete(username); ok {
		t.(*time.Timer).Stop()
	}

	// One peerList per connect rather than two (see broadcastStatusTo).
	safeGo("announcePresence", func() {
		peers := peerList(username)
		broadcastStatusTo(username, "online", peers)
		sendOnlineStatuses(client, peers)
	})

	defer func() {
		mu.Lock()
		left := removeClientLocked(username, client)
		if left == 0 {
			// Defer "offline" by a few seconds. A page reload closes the
			// socket and immediately opens another, and an instant
			// broadcast would show everyone an offline→online flicker; a
			// reconnect inside the window cancels this timer.
			uname := username
			timer := time.AfterFunc(6*time.Second, func() {
				pendingOffline.Delete(uname)
				mu.RLock()
				stillGone := len(clients[uname]) == 0
				mu.RUnlock()
				if stillGone {
					broadcastStatus(uname, "offline")
				}
			})
			pendingOffline.Store(username, timer)
			// Note: we intentionally do NOT send call-end here on WS disconnect.
			// A brief WS reconnect (network hiccup, background app management)
			// would otherwise kill an active call. Client-side 45s timeouts
			// handle the case where a participant truly disappears.
			delete(callMap, username)
			for caller, callee := range callMap {
				if callee == username {
					delete(callMap, caller)
					break
				}
			}
		}
		mu.Unlock()
		client.close() // stops writeLoop and closes the socket, idempotently
		log.Println(username, "disconnected (", left, "devices left)")
	}()

	deliverPending(username, client)
	flushCallSignals(username, client)

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		var msg Message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		msg.From = username

		// Client-supplied fields are bounded: an over-long id is replaced
		// with a generated one, and text bodies are capped below. A media
		// body is a URL and short by construction.
		if len(msg.ID) > 64 {
			msg.ID = generateID()
		}
		if msg.Type == "message" || msg.Type == "" {
			// len() counts bytes, which would halve the limit for any
			// non-Latin script; counting runes keeps it language-neutral.
			if utf8.RuneCountInString(msg.Body) > maxMessageRunes {
				sendACKReason(client, msg.ID, "failed", 0, "too_long", 0)
				continue
			}
		}

		switch msg.Type {
		case "ping":
			continue
		case "typing":
			d, _ := json.Marshal(msg)
			if isGroup(msg.To) {
				// A group has no entry in clients, so a typing event
				// addressed to the group id would reach nobody; it is
				// expanded over the members instead.
				members := groupMembers(msg.To)
				mu.RLock()
				for _, m := range members {
					if m == username {
						continue
					}
					for _, c := range clients[m] {
						c.send(d)
					}
				}
				mu.RUnlock()
			} else {
				mu.RLock()
				for _, c := range clients[msg.To] {
					c.send(d)
				}
				mu.RUnlock()
			}
			continue
		case "read":
			handleRead(msg)
			continue
		case "reaction":
			// Server-side allow-list: msg.Emoji is rendered client-side
			// straight into innerHTML/onclick attributes, so an unvalidated
			// value here is a stored-XSS vector reachable by any logged-in
			// user against any other (see audit). Mirror the picker in
			// index.html exactly — reject anything not on this exact list.
			if !allowedReactionEmoji[msg.Emoji] {
				continue
			}
			// The only WS type that writes to the database and fans out.
			// Dropping silently is safe: the client applied the reaction
			// optimistically and the next sync corrects it.
			if reactionLimiter.isBlocked(username) {
				continue
			}
			reactionLimiter.recordFailure(username)
			// Toggling mirrors the client: the same reaction again removes
			// it. Reactions are restricted to one's own conversations, or
			// the table can be filled with reactions to arbitrary ids.
			var s, rcp string
			if db.QueryRow(`SELECT sender, recipient FROM messages WHERE id=$1`,
				msg.ID).Scan(&s, &rcp) != nil {
				continue
			}
			if isGroup(rcp) {
				if !isGroupMember(rcp, username) {
					continue
				}
			} else if s != username && rcp != username {
				continue
			}
			db.Exec(`INSERT INTO reactions(msg_id, username, emoji) VALUES($1,$2,$3)
			         ON CONFLICT (msg_id, username, emoji) DO UPDATE
			         SET removed = NOT reactions.removed, created_at = NOW(),
			             rseq = nextval(pg_get_serial_sequence('reactions','rseq'))`,
				msg.ID, username, msg.Emoji)

			// Echo to the author's other devices — the originating tab
			// already applied it optimistically. Routing below reaches the
			// peer only, so without this echo a reaction made in one tab
			// stays invisible in another until a reload.
			echo, _ := json.Marshal(msg)
			mu.RLock()
			for _, c := range clients[username] {
				if c != client {
					c.send(echo)
				}
			}
			mu.RUnlock()
		}

		if msg.ID == "" {
			msg.ID = generateID()
		}

		// Anything that isn't a normal user "message" (calls, reactions, etc.)
		// is routed without persistence.
		if msg.Type != "" && msg.Type != "message" {
			routeMessageNoSave(msg)
			continue
		}

		// A log line per message is a syscall under the global log mutex on
		// the hot path, plus the container log driver's work: one load run
		// produced 772000 lines and 35 MB. It carries metadata only, and
		// stays behind a flag for delivery debugging.
		if debugMessageLog {
			log.Printf("MSG %s -> %s [%s]", msg.From, msg.To, msg.ID)
		}
		routeMessage(msg, client)
	}
}

// removeClientLocked drops a connection from clients and returns how many
// remain for that user. Call it under mu.Lock().
//
// The list is rebuilt as a new slice rather than edited in place. Broadcasts
// take the slice header under RLock and iterate it without the lock, so
// in-place editing has them reading the very cells a disconnect is
// rewriting — a data race under the Go memory model (caught by -race in
// TestClientsSliceNoRaceOnDisconnect) and, in practice, a skipped or
// duplicated recipient. Copy-on-write keeps the published array immutable,
// so a reader holding the old header sees a consistent snapshot.
func removeClientLocked(username string, client *Client) int {
	conns := clients[username]
	out := make([]*Client, 0, len(conns))
	for _, c := range conns {
		if c != client {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		delete(clients, username)
		return 0
	}
	clients[username] = out
	return len(out)
}

// snapshotClients copies a user's connections for lock-free iteration.
func snapshotClients(username string) []*Client {
	mu.RLock()
	defer mu.RUnlock()
	return append([]*Client(nil), clients[username]...)
}

var callMap = map[string]string{}

// pendingOffline maps a username to the timer of its deferred "offline"
// broadcast, which suppresses presence flicker across a page reload.
var pendingOffline sync.Map

// knownUsers caches existing usernames to avoid a SELECT per message.
// Entries expire after knownUserTTL: without expiry, a user deleted directly
// in the database counts as existing until the server restarts, and messages
// keep being stored for nobody.
const knownUserTTL = 10 * time.Minute

var knownUsers sync.Map // username -> time.Time when cached

// forgetUser evicts immediately, so admin deletions do not wait out the TTL.
func forgetUser(username string) {
	knownUsers.Delete(username)
}

// hasContactWith reports whether two users share context — a conversation
// or a group — which is what gates presence visibility.
func hasContactWith(me, other string) bool {
	var ok bool
	db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM messages
		WHERE (sender=$1 AND recipient=$2) OR (sender=$2 AND recipient=$1)
		LIMIT 1
	)`, me, other).Scan(&ok)
	if ok {
		return true
	}
	db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM group_members a
		JOIN group_members b ON a.group_id = b.group_id
		WHERE a.username=$1 AND b.username=$2
		LIMIT 1
	)`, me, other).Scan(&ok)
	return ok
}

func userExists(username string) bool {
	if v, ok := knownUsers.Load(username); ok {
		if cachedAt, ok2 := v.(time.Time); ok2 && time.Since(cachedAt) < knownUserTTL {
			return true
		}
		knownUsers.Delete(username) // expired, re-check the database
	}
	var exists bool
	db.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)", username).Scan(&exists)
	if exists {
		knownUsers.Store(username, time.Now())
	}
	return exists
}

func routeMessageNoSave(msg Message) {
	// Track call state so we can send call-end if a participant disconnects
	// before the call is properly terminated (browser closed, app killed, etc.)
	mu.Lock()
	switch msg.Type {
	case "call-offer":
		// Ignore duplicate call-offer from the same caller — prevents glare
		// when both sides call simultaneously, which causes both sessions to
		// destroy each other. The first offer wins; subsequent ones are dropped.
		if _, exists := callMap[msg.From]; exists {
			// mu covers both clients and callMap, so an early return
			// holding it would freeze the whole server.
			mu.Unlock()
			return
		}
		callMap[msg.From] = msg.To
	case "call-end", "call-reject":
		delete(callMap, msg.From)
		delete(callMap, msg.To)
	}
	mu.Unlock()

	if isGroup(msg.To) {
		// Relay untyped events (reactions and the like) to group members.
		data, _ := json.Marshal(msg)
		members := groupMembers(msg.To)
		mu.RLock()
		for _, m := range members {
			if m == msg.From {
				continue
			}
			for _, c := range clients[m] {
				c.send(data)
			}
		}
		mu.RUnlock()
		return
	}
	recipients := snapshotClients(msg.To)
	data, _ := json.Marshal(msg)
	if len(recipients) == 0 {
		switch msg.Type {
		case "call-offer":
			queueCallSignal(msg.To, data, true)
			safeGo("notifyCallPush", func() { notifyCallPush(msg.To, msg.From) })
		case "call-ice":
			queueCallSignal(msg.To, data, false)
		case "call-end", "call-reject":
			clearCallQueue(msg.To)
		}
		return
	}
	for _, r := range recipients {
		r.send(data)
	}
}

// handleRead marks all messages from msg.To -> msg.From as read,
// and notifies the original sender's devices.
func handleRead(msg Message) {
	if isGroup(msg.To) {
		return // per-user read receipts for groups are not implemented
	}
	// msg.From is the reader; msg.To is the original sender (peer).
	reader := msg.From
	peer := msg.To
	rows, err := db.Query(
		`UPDATE messages SET read=true
		 WHERE sender=$1 AND recipient=$2 AND read=false
		 RETURNING id`, peer, reader)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}
	// Tell the original sender (peer) their messages were read.
	conns := snapshotClients(peer)
	for _, id := range ids {
		ack := Message{Type: "ack", ID: id, Body: "read"}
		data, _ := json.Marshal(ack)
		for _, c := range conns {
			c.send(data)
		}
	}
}

// routeMessage persists one message and pushes it live to every connected
// device of the recipient AND every OTHER connected device of the sender
// (so the sender's other devices see their own outgoing message instantly,
// without re-deriving it from /history). The server is the single source
// of truth — one stored row serves every device identically.
func routeMessage(msg Message, sender *Client) {
	if isGroup(msg.To) {
		routeGroupMessage(msg, sender)
		return
	}
	// Messages to non-existent recipients are refused, or any client can
	// inflate the database with rows addressed to nobody.
	if !userExists(msg.To) {
		sendACKReason(sender, msg.ID, "failed", 0, "no_user", 0)
		return
	}
	seq, createdAt, err := saveMessage(msg)
	if err != nil {
		log.Println("saveMessage FAILED:", err)
		sendACKToAll(sender.Username, msg.ID, "failed", 0)
		return
	}
	msg.Seq = seq
	msg.CreatedAt = createdAt
	data, _ := json.Marshal(msg)

	recipients := snapshotClients(msg.To)
	senderOtherDevices := snapshotClients(sender.Username)

	delivered := false
	for _, c := range recipients {
		if c.send(data) == nil {
			delivered = true
		}
	}
	for _, c := range senderOtherDevices {
		if c != sender {
			c.send(data)
		}
	}

	if delivered {
		markDelivered(msg.ID) // batched, not one query per message
	}
	status := "sent"
	if delivered {
		status = "delivered"
	} else if !chatMuted(msg.To, msg.From) {
		// The recipient has no live socket, so notify by Web Push. A muted
		// conversation produces none: the client cannot cancel a push, the
		// phone has already buzzed. The message status is unaffected — not
		// notifying and not delivering are different things, and delivery
		// is picked up by the seq cursor.
		safeGo("notifyOfflinePush", func() { notifyOfflinePush(msg.To, msg.From, msg.MediaType) })
	}
	sendACK(sender, msg.ID, status, seq, createdAt)
}

// routeGroupMessage is routeMessage fanned out over a group. The delivered
// flag is per message rather than per member, so for groups it is set at
// once; offline members pick the message up through the ordinary history
// sync by seq.
func routeGroupMessage(msg Message, sender *Client) {
	if !isGroupMember(msg.To, sender.Username) {
		sendACKReason(sender, msg.ID, "failed", 0, "not_member", 0)
		return
	}
	seq, createdAt, err := saveMessage(msg)
	if err != nil {
		log.Println("saveMessage FAILED:", err)
		sendACKToAll(sender.Username, msg.ID, "failed", 0)
		return
	}
	markDelivered(msg.ID)
	msg.Seq = seq
	msg.CreatedAt = createdAt
	data, _ := json.Marshal(msg)

	members := groupMembers(msg.To)
	gname := groupName(msg.To)
	type target struct {
		online bool
		conns  []*Client
	}
	targets := map[string]target{}
	mu.RLock()
	for _, m := range members {
		// Copy the slice: the map outlives the RUnlock and a disconnect
		// rebuilds the connection list (see removeClientLocked).
		conns := append([]*Client(nil), clients[m]...)
		targets[m] = target{online: len(conns) > 0, conns: conns}
	}
	mu.RUnlock()

	for m, t := range targets {
		for _, c := range t.conns {
			if c != sender {
				c.send(data)
			}
		}
		// For a group the preference key is the group id: people mute a
		// noisy chat, not each of its members.
		if m != sender.Username && !t.online && !chatMuted(m, msg.To) {
			mm := m
			safeGo("notifyOfflinePush", func() {
				notifyOfflinePush(mm, msg.From+" in \""+gname+"\"", msg.MediaType)
			})
		}
	}
	sendACK(sender, msg.ID, "delivered", seq, createdAt)
}

// saveMessage inserts and returns the assigned seq + created_at(ms). The
// body is encrypted at rest with the server's key.
func saveMessage(msg Message) (int64, int64, error) {
	if msg.Type != "" && msg.Type != "message" {
		return 0, 0, nil
	}
	body := msg.Body
	if msg.MediaType == "" {
		body = encryptBody(body) // media bodies are URLs, not secret content
	}
	// The insert itself goes to the writers in msgwriter.go, which merge
	// concurrent messages into one multi-row INSERT. The call stays
	// synchronous: the caller still has seq and created_at before sending
	// the ACK, and one sender's ordering is preserved.
	//
	// Buffered for one value so the writer never blocks on the waiter.
	reply := make(chan saveResult, 1)

	// Both waits are bounded. Blocking unconditionally means that when the
	// database is unreachable the queue fills and the WS handler goroutine
	// stalls forever: the client gets neither the message nor an error, and
	// the connection hangs dead because the read loop cannot resume until
	// routeMessage returns. With a timeout the sender gets ACK failed and
	// retries with the same id, which ON CONFLICT makes idempotent, so the
	// timeout creates neither duplicates nor losses.
	select {
	case msgQueue <- saveRequest{msg: msg, body: body, reply: reply}:
	case <-time.After(saveEnqueueTimeout):
		statSaveTimeouts.Add(1)
		return 0, 0, errSaveOverloaded
	}
	select {
	case res := <-reply:
		if res.err != nil {
			return 0, 0, res.err
		}
		statMessagesSaved.Add(1)
		return res.seq, res.createdAt, nil
	case <-time.After(saveReplyTimeout):
		statSaveTimeouts.Add(1)
		return 0, 0, errSaveTimeout
	}
}

func sendACK(client *Client, msgID, status string, seq, createdAt int64) {
	sendACKReason(client, msgID, status, seq, "", createdAt)
}

func sendACKReason(client *Client, msgID, status string, seq int64, reason string, createdAt int64) {
	if client == nil {
		return
	}
	// The ACK carries the server-side timestamp so the sender displays the
	// same time as the recipient; a device clock that drifts would
	// otherwise show the author a different send time from everyone else.
	ack := Message{Type: "ack", ID: msgID, Body: status, Seq: seq, Reason: reason, CreatedAt: createdAt}
	data, _ := json.Marshal(ack)
	client.send(data)
}

// sendACKToAll notifies every connected device of username (e.g. the
// sender's other devices) about a status change for one message.
func sendACKToAll(username, msgID, status string, seq int64) {
	conns := snapshotClients(username)
	ack := Message{Type: "ack", ID: msgID, Body: status, Seq: seq}
	data, _ := json.Marshal(ack)
	for _, c := range conns {
		c.send(data)
	}
}
