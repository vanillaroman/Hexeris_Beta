package main

// Per-conversation settings: mute, archive and clearing one's own view.
//
// All of these belong to a pair (who is looking, at which conversation)
// rather than to the conversation itself: one member may mute a work group
// while another does not. Hence one table keyed by (username, peer), where
// peer is either a username or a group id.
//
// They live on the server rather than in local storage because they have
// consequences beyond the tab. Push is sent by the server, and a client that
// does not want a notification cannot cancel one — the phone has already
// buzzed. Archive and clearing must also look the same on a phone and a
// desktop, or a list tidied on one device has to be tidied again on the other.
//
// Clearing (cleared_seq) does not delete messages. It raises a personal
// visibility boundary: earlier messages are no longer served to this user,
// while the peer keeps the conversation and an investigation can still reach
// it. For a corporate messenger that is the only honest behaviour — "delete
// for everyone" would let any employee erase evidence.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sync"
)

// chatPref is what the client knows about one conversation.
//
// ArchivedAt is the archiving moment in milliseconds, 0 when not archived. It
// orders the archive section: sorting archived chats by recency would let any
// incoming reply reshuffle exactly the conversations nobody is waiting on.
type chatPref struct {
	Muted      bool  `json:"muted"`
	Archived   bool  `json:"archived"`
	ClearedSeq int64 `json:"cleared_seq"`
	ArchivedAt int64 `json:"archived_at"`
}

// archivedAtMillis turns NULL into 0: unarchived rows carry no timestamp.
func archivedAtMillis(t sql.NullTime) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.UnixMilli()
}

// The mute check runs for every message to an offline recipient, which in a
// group of a hundred means a hundred checks per message. Querying the
// database each time is not an option, so the whole set lives in memory: a
// user has only a handful of muted rows, and the set is tens of kilobytes
// even for a large company.
var (
	mutedMu  sync.RWMutex
	mutedSet map[string]struct{} // key: username + "\x00" + peer
)

func mutedKey(username, peer string) string { return username + "\x00" + peer }

// loadMutedCache fills the set at startup. A failure is not fatal: an empty
// cache means "nobody muted anything", which is a degraded feature rather
// than a broken server.
func loadMutedCache() {
	set := map[string]struct{}{}
	rows, err := db.Query(`SELECT username, peer FROM chat_prefs WHERE muted`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var u, p string
			if rows.Scan(&u, &p) == nil {
				set[mutedKey(u, p)] = struct{}{}
			}
		}
	}
	mutedMu.Lock()
	mutedSet = set
	mutedMu.Unlock()
}

func setMutedCache(username, peer string, muted bool) {
	mutedMu.Lock()
	if mutedSet == nil {
		mutedSet = map[string]struct{}{}
	}
	if muted {
		mutedSet[mutedKey(username, peer)] = struct{}{}
	} else {
		delete(mutedSet, mutedKey(username, peer))
	}
	mutedMu.Unlock()
}

// chatMuted reports whether username has muted peer.
func chatMuted(username, peer string) bool {
	mutedMu.RLock()
	_, ok := mutedSet[mutedKey(username, peer)]
	mutedMu.RUnlock()
	return ok
}

// clearedSeqs returns the user's cleared conversations as peer → cleared_seq.
// An empty map is the common case and costs nothing to filter against.
func clearedSeqs(username string) map[string]int64 {
	out := map[string]int64{}
	rows, err := db.Query(`SELECT peer, cleared_seq FROM chat_prefs
	                       WHERE username=$1 AND cleared_seq > 0`, username)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var s int64
		if rows.Scan(&p, &s) == nil {
			out[p] = s
		}
	}
	return out
}

// chatPrefsHandler serves all of a user's settings on GET and updates one on
// POST.
//
//	GET  /chats/prefs            → {"bob": {"muted":true,...}, ...}
//	POST /chats/prefs            {"peer":"bob","muted":true}
//
// POST fields are pointers so an absent field means "leave alone" rather than
// "switch off": otherwise archiving a conversation would silently unmute it.
func chatPrefsHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Method == http.MethodGet {
		out := map[string]chatPref{}
		rows, err := db.Query(`SELECT peer, muted, archived, cleared_seq, archived_at
		                       FROM chat_prefs WHERE username=$1`, username)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			var c chatPref
			var at sql.NullTime
			if rows.Scan(&p, &c.Muted, &c.Archived, &c.ClearedSeq, &at) == nil {
				c.ArchivedAt = archivedAtMillis(at)
				out[p] = c
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Peer     string `json:"peer"`
		Muted    *bool  `json:"muted"`
		Archived *bool  `json:"archived"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Peer == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Muted == nil && req.Archived == nil {
		http.Error(w, "nothing to update", http.StatusBadRequest)
		return
	}
	if req.Peer == username {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// The setting itself is harmless, but creating rows for arbitrary
	// strings lets any client grow the table quietly.
	if !chatPeerExists(req.Peer, username) {
		http.Error(w, "no such conversation", http.StatusNotFound)
		return
	}

	// UPSERT with COALESCE: an omitted field keeps its previous value and an
	// insert takes the default. archived_at moves only on the transition
	// into the archive — leaving it untouched when only mute changes, and
	// clearing it on un-archive so the next archiving lands at the end of
	// the section instead of returning to its old place.
	var cur chatPref
	var at sql.NullTime
	err := db.QueryRow(`
		INSERT INTO chat_prefs (username, peer, muted, archived, archived_at)
		VALUES ($1, $2, COALESCE($3, FALSE), COALESCE($4, FALSE),
		        CASE WHEN COALESCE($4, FALSE) THEN NOW() END)
		ON CONFLICT (username, peer) DO UPDATE SET
			muted       = COALESCE($3, chat_prefs.muted),
			archived    = COALESCE($4, chat_prefs.archived),
			archived_at = CASE
				WHEN $4 IS NULL THEN chat_prefs.archived_at
				WHEN $4         THEN COALESCE(chat_prefs.archived_at, NOW())
			END,
			updated_at  = NOW()
		RETURNING muted, archived, cleared_seq, archived_at`,
		username, req.Peer, req.Muted, req.Archived).
		Scan(&cur.Muted, &cur.Archived, &cur.ClearedSeq, &at)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	cur.ArchivedAt = archivedAtMillis(at)
	setMutedCache(username, req.Peer, cur.Muted)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cur)
}

// chatClearHandler raises the caller's personal visibility boundary to the
// conversation's current maximum and answers with the new cleared_seq, so the
// client does not have to guess what disappeared.
func chatClearHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Peer string `json:"peer"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Peer == "" || req.Peer == username {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// The peer no longer exists — and it is EXACTLY that row a person needs to
	// remove from their list. A 404 used to stand here, so it came out the other
	// way round: the conversation with a deleted employee was the only one that
	// could not be deleted. The client said "Could not delete — check your
	// connection", blaming the network for an answer the server gave definitely.
	//
	// There is nothing to hide here: deleting an account erases all of its
	// correspondence (admin.go, step 4), so no visibility boundary is needed — the
	// server side is empty. We answer with success and do NOT write a chat_prefs
	// row: otherwise guessing names would give an unbounded write to the table.
	//
	// The oracle disappears along with it: a 404 versus a 200 used to tell the
	// asker whether such a name exists. The answer is now the same in both cases.
	if !chatPeerExists(req.Peer, username) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"cleared_seq": 0, "gone": true})
		return
	}

	// The boundary is the conversation's maximum seq at this moment. A
	// message arriving afterwards gets a higher seq and stays visible:
	// clearing is not blocking.
	var maxSeq sql.NullInt64
	if isGroup(req.Peer) {
		db.QueryRow(`SELECT MAX(seq) FROM messages WHERE recipient=$1`, req.Peer).Scan(&maxSeq)
	} else {
		db.QueryRow(`SELECT MAX(seq) FROM messages
		             WHERE (sender=$1 AND recipient=$2) OR (sender=$2 AND recipient=$1)`,
			username, req.Peer).Scan(&maxSeq)
	}

	// GREATEST keeps a concurrent clear from lowering the boundary again.
	var cleared int64
	err := db.QueryRow(`
		INSERT INTO chat_prefs (username, peer, cleared_seq)
		VALUES ($1, $2, $3)
		ON CONFLICT (username, peer) DO UPDATE SET
			cleared_seq = GREATEST(chat_prefs.cleared_seq, $3),
			updated_at  = NOW()
		RETURNING cleared_seq`, username, req.Peer, maxSeq.Int64).Scan(&cleared)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"cleared_seq": cleared})
}

// chatKeyFor returns the key under which a user sees this message in their
// chat list: the group id for groups, otherwise the other participant. It
// must match what the client sends to /chats/prefs, or mute and clearing miss
// the conversation.
func chatKeyFor(username, from, to string) string {
	if isGroup(to) {
		return to
	}
	if from == username {
		return to
	}
	return from
}

// chatPeerExists reports whether the peer exists — for groups, whether the
// caller is a member, so a group id cannot be probed for existence.
func chatPeerExists(peer, username string) bool {
	if isGroup(peer) {
		return isGroupMember(peer, username)
	}
	var exists bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE username=$1)`, peer).Scan(&exists)
	return exists
}
