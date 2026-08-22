package main

// Conversation settings: mute, archive and clearing (requires
// TEST_DATABASE_URL).
//
// Beyond "it saved and loaded", these cover the three ways the feature can
// look working while being broken:
//   1. a partial update must not reset a neighbouring field;
//   2. the mute cache must agree with the database, or push is sent anyway;
//   3. clearing must hide the conversation only for whoever cleared it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func prefsGet(t *testing.T, token string) map[string]chatPref {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/chats/prefs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	chatPrefsHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /chats/prefs: status %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]chatPref
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func prefsPost(t *testing.T, h http.HandlerFunc, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:1"
	h(rr, req)
	return rr
}

// historyFor returns one conversation as the token's owner sees it.
func historyFor(t *testing.T, token, peer string) []map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/history?peer="+peer+"&limit=500", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:1"
	historyHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/history: status %d: %s", rr.Code, rr.Body.String())
	}
	var out []map[string]any
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func TestIntegrationChatPrefsPartialUpdate(t *testing.T) {
	setupIntegration(t)
	me, peer := uniqueName("cp_me"), uniqueName("cp_peer")
	seedUsers(t, me, peer)
	tok, _ := generateToken(me)

	// An empty set is {}, not an error: a new user has no settings.
	if got := prefsGet(t, tok); len(got) != 0 {
		t.Fatalf("a new user already has settings: %v", got)
	}

	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "muted": true}); rr.Code != http.StatusOK {
		t.Fatalf("mute: status %d: %s", rr.Code, rr.Body.String())
	}
	// Archive through the same form but without the muted field. Parsed as
	// plain bools, an absent field becomes false and archiving would
	// silently unmute the conversation.
	rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("archive: status %d: %s", rr.Code, rr.Body.String())
	}
	var got chatPref
	json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.Muted || !got.Archived {
		t.Fatalf("partial update overwrote a neighbouring field: %+v", got)
	}
	if all := prefsGet(t, tok); !all[peer].Muted || !all[peer].Archived {
		t.Fatalf("GET returns something other than what was written: %+v", all[peer])
	}

	// The archiving timestamp is set; the client orders the archive by it.
	stamp := got.ArchivedAt
	if stamp == 0 {
		t.Fatal("archived_at is unset — the archive would sort arbitrarily")
	}
	// Changing mute must not move the mark, or an unrelated setting would
	// reshuffle the archive.
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "muted": false})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt != stamp {
		t.Fatalf("mute moved the archiving time: %d → %d", stamp, got.ArchivedAt)
	}
	// Re-archiving an archived conversation keeps the order stable.
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": true})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt != stamp {
		t.Fatalf("re-archiving moved the mark: %d → %d", stamp, got.ArchivedAt)
	}
	// Un-archiving clears it, so the next archiving gets a fresh time and
	// lands at the top of the section instead of its old place.
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": false})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt != 0 {
		t.Fatalf("un-archiving left the mark at %d", got.ArchivedAt)
	}
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": true})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt <= stamp {
		t.Fatalf("re-archiving did not refresh the mark: %d, was %d", got.ArchivedAt, stamp)
	}

	// A request with no fields is an error, not a silent no-op.
	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer}); rr.Code != http.StatusBadRequest {
		t.Fatalf("request with no fields: want 400, got %d", rr.Code)
	}
	// No row is created for a peer that does not exist.
	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": uniqueName("cp_ghost"), "muted": true}); rr.Code != http.StatusNotFound {
		t.Fatalf("non-existent peer: want 404, got %d", rr.Code)
	}
	// Without a token, nothing.
	rrNoAuth := httptest.NewRecorder()
	chatPrefsHandler(rrNoAuth, httptest.NewRequest(http.MethodGet, "/chats/prefs", nil))
	if rrNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("without a token: want 401, got %d", rrNoAuth.Code)
	}
}

// The cache that decides a push's fate must agree with the database. A
// divergence is invisible in every API response: the setting reads as saved
// while notifications keep arriving.
func TestIntegrationChatMuteCache(t *testing.T) {
	setupIntegration(t)
	me, peer, other := uniqueName("cm_me"), uniqueName("cm_peer"), uniqueName("cm_other")
	seedUsers(t, me, peer, other)
	tok, _ := generateToken(me)

	if chatMuted(me, peer) {
		t.Fatal("the conversation is muted before anyone muted it")
	}
	prefsPost(t, chatPrefsHandler, "/chats/prefs", tok, map[string]any{"peer": peer, "muted": true})
	if !chatMuted(me, peer) {
		t.Fatal("cache not updated after mute — push would still be sent")
	}
	// Exactly one conversation is muted, not everything incoming.
	if chatMuted(me, other) {
		t.Fatal("muting one conversation muted another")
	}
	// And for exactly one user.
	if chatMuted(peer, me) {
		t.Fatal("mute applied to the peer instead of whoever set it")
	}

	// A server restart rebuilds the cache from the database.
	mutedMu.Lock()
	mutedSet = nil
	mutedMu.Unlock()
	loadMutedCache()
	if !chatMuted(me, peer) {
		t.Fatal("mute was lost across a restart")
	}

	prefsPost(t, chatPrefsHandler, "/chats/prefs", tok, map[string]any{"peer": peer, "muted": false})
	if chatMuted(me, peer) {
		t.Fatal("unmute did not clear the cache entry")
	}
}

// Clearing hides the conversation for whoever cleared it and nobody else;
// messages sent afterwards are visible again.
func TestIntegrationChatClear(t *testing.T) {
	setupIntegration(t)
	me, peer := uniqueName("cc_me"), uniqueName("cc_peer")
	seedUsers(t, me, peer)
	myTok, _ := generateToken(me)
	peerTok, _ := generateToken(peer)

	for i, body := range []string{"first", "second", "third"} {
		msg := Message{Type: "message", ID: uniqueName("cc_msg"), From: me, To: peer, Body: body}
		if i%2 == 1 {
			msg.From, msg.To = peer, me // in both directions
		}
		if _, _, err := saveMessage(msg); err != nil {
			t.Fatal("saveMessage:", err)
		}
	}
	if n := len(historyFor(t, myTok, peer)); n != 3 {
		t.Fatalf("want 3 messages before clearing, got %d", n)
	}

	rr := prefsPost(t, chatClearHandler, "/chats/clear", myTok, map[string]any{"peer": peer})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear: status %d: %s", rr.Code, rr.Body.String())
	}
	var cleared map[string]int64
	json.Unmarshal(rr.Body.Bytes(), &cleared)
	if cleared["cleared_seq"] == 0 {
		t.Fatal("the boundary stayed at zero — clearing hid nothing")
	}

	if n := len(historyFor(t, myTok, peer)); n != 0 {
		t.Fatalf("%d messages still visible after clearing", n)
	}
	// The peer keeps the conversation: clearing is not deleting for all.
	if n := len(historyFor(t, peerTok, me)); n != 3 {
		t.Fatalf("clearing affected the peer: %d of 3 remain", n)
	}
	// And in the database: messages must survive for an investigation.
	var inDB int
	db.QueryRow(`SELECT COUNT(*) FROM messages
	             WHERE (sender=$1 AND recipient=$2) OR (sender=$2 AND recipient=$1)`,
		me, peer).Scan(&inDB)
	if inDB != 3 {
		t.Fatalf("clearing deleted rows: %d of 3 remain", inDB)
	}

	// A new message gets a higher seq and shows again: clearing is a
	// boundary in the past, not a block on the peer.
	if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("cc_msg"),
		From: peer, To: me, Body: "after clearing"}); err != nil {
		t.Fatal("saveMessage:", err)
	}
	h := historyFor(t, myTok, peer)
	if len(h) != 1 || h[0]["body"] != "after clearing" {
		t.Fatalf("want one new message after clearing, got %v", h)
	}

	// Clearing again must not lower the boundary.
	before := cleared["cleared_seq"]
	rr = prefsPost(t, chatClearHandler, "/chats/clear", myTok, map[string]any{"peer": peer})
	json.Unmarshal(rr.Body.Bytes(), &cleared)
	if cleared["cleared_seq"] < before {
		t.Fatalf("the second clear lowered the boundary: %d -> %d", before, cleared["cleared_seq"])
	}
}

// A cleared conversation must not come back out of the offline queue on the
// next connect, while its rows must still be marked delivered: otherwise they
// stay pending forever, arrive on every reconnect, and the sender never sees
// delivery.
func TestIntegrationChatClearOfflineQueue(t *testing.T) {
	setupIntegration(t)
	me, peer := uniqueName("cq_me"), uniqueName("cq_peer")
	seedUsers(t, me, peer)
	myTok, _ := generateToken(me)

	for i := 0; i < 3; i++ {
		if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("cq_msg"),
			From: peer, To: me, Body: "offline"}); err != nil {
			t.Fatal("saveMessage:", err)
		}
	}
	if got := pendingCount(t, me); got != 3 {
		t.Fatalf("want 3 undelivered, got %d", got)
	}

	if rr := prefsPost(t, chatClearHandler, "/chats/clear", myTok,
		map[string]any{"peer": peer}); rr.Code != http.StatusOK {
		t.Fatalf("clear: status %d: %s", rr.Code, rr.Body.String())
	}

	c := testClient(me)
	deliverPending(me, c)

	if got := pendingCount(t, me); got != 0 {
		t.Fatalf("%d still undelivered after clearing — the queue never drains", got)
	}
	// Nothing went to the socket: the messages are hidden, not re-delivered.
	select {
	case data := <-c.out:
		t.Fatalf("a cleared message reached the client anyway: %s", data)
	default:
	}
}

// Clearing a group for one member leaves it intact for everyone else, and a
// group one does not belong to cannot be cleared at all.
func TestIntegrationChatClearGroupScope(t *testing.T) {
	setupIntegration(t)
	owner, member, outsider := uniqueName("cg_own"), uniqueName("cg_mem"), uniqueName("cg_out")
	seedUsers(t, owner, member, outsider)
	gid := makeGroup(t, owner, member, uniqueName("Cleared "))
	ownerTok, _ := generateToken(owner)
	memberTok, _ := generateToken(member)
	outsiderTok, _ := generateToken(outsider)

	for _, body := range []string{"one", "two"} {
		if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("cg_msg"),
			From: owner, To: gid, Body: body}); err != nil {
			t.Fatal("saveMessage:", err)
		}
	}

	// An outsider can neither learn about the group nor clear it.
	if rr := prefsPost(t, chatClearHandler, "/chats/clear", outsiderTok,
		map[string]any{"peer": gid}); rr.Code != http.StatusNotFound {
		t.Fatalf("outsider cleared the group: want 404, got %d", rr.Code)
	}
	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", outsiderTok,
		map[string]any{"peer": gid, "muted": true}); rr.Code != http.StatusNotFound {
		t.Fatalf("outsider muted the group: want 404, got %d", rr.Code)
	}

	if rr := prefsPost(t, chatClearHandler, "/chats/clear", memberTok,
		map[string]any{"peer": gid}); rr.Code != http.StatusOK {
		t.Fatalf("member could not clear the group: status %d: %s", rr.Code, rr.Body.String())
	}
	if n := len(historyFor(t, memberTok, gid)); n != 0 {
		t.Fatalf("the member still sees %d messages after clearing", n)
	}
	if n := len(historyFor(t, ownerTok, gid)); n != 2 {
		t.Fatalf("one member's clear affected the others: admin sees %d of 2", n)
	}
}
