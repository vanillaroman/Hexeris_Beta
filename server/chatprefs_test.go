package main

// Conversation preferences: mute, archive, clear (requires TEST_DATABASE_URL).
//
// More than "saved/read back" is checked here — three things that can make the
// feature look like it works while it does not:
//   1. a partial update does not reset a neighbouring field;
//   2. the mute cache agrees with the database (or the push goes out anyway);
//   3. a clear hides the correspondence only from whoever cleared it.

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

// historyFor — one conversation's history as the token owner sees it.
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

	// An empty set is {}, not an error: a new user has no preferences.
	if got := prefsGet(t, tok); len(got) != 0 {
		t.Fatalf("a new user already has preferences: %v", got)
	}

	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "muted": true}); rr.Code != http.StatusOK {
		t.Fatalf("mute: status %d: %s", rr.Code, rr.Body.String())
	}
	// Archive with THE SAME request shape but without the muted field. If the
	// fields were parsed as plain bools, a missing one would become false and
	// archiving would silently unmute the conversation.
	rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("archive: status %d: %s", rr.Code, rr.Body.String())
	}
	var got chatPref
	json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.Muted || !got.Archived {
		t.Fatalf("a partial update wiped a neighbouring field: %+v", got)
	}
	if all := prefsGet(t, tok); !all[peer].Muted || !all[peer].Archived {
		t.Fatalf("GET returns something other than what was written: %+v", all[peer])
	}

	// The archiving timestamp is set — the client orders the "Archive" section
	// by it.
	stamp := got.ArchivedAt
	if stamp == 0 {
		t.Fatal("archived_at is not set — the archive will sort arbitrarily")
	}
	// Editing mute does NOT move the stamp: otherwise a conversation would jump
	// around the archive because of a setting unrelated to archiving.
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "muted": false})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt != stamp {
		t.Fatalf("mute moved the archiving time: %d -> %d", stamp, got.ArchivedAt)
	}
	// Re-archiving an archived one does not move it either — the order is stable.
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": true})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt != stamp {
		t.Fatalf("re-archiving moved the stamp: %d -> %d", stamp, got.ArchivedAt)
	}
	// Leaving the archive clears the stamp: the next archiving gets a fresh time
	// and rises to the top of the section instead of returning to its old place.
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": false})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt != 0 {
		t.Fatalf("leaving the archive left the stamp %d", got.ArchivedAt)
	}
	rr = prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer, "archived": true})
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.ArchivedAt <= stamp {
		t.Fatalf("re-archiving did not refresh the stamp: %d, was %d", got.ArchivedAt, stamp)
	}

	// A request with no fields at all is an error, not a silent no-op.
	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": peer}); rr.Code != http.StatusBadRequest {
		t.Fatalf("request with no fields: expected 400, got %d", rr.Code)
	}
	// A non-existent peer: no row is created.
	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", tok,
		map[string]any{"peer": uniqueName("cp_ghost"), "muted": true}); rr.Code != http.StatusNotFound {
		t.Fatalf("non-existent peer: expected 404, got %d", rr.Code)
	}
	// Without a token, nothing.
	rrNoAuth := httptest.NewRecorder()
	chatPrefsHandler(rrNoAuth, httptest.NewRequest(http.MethodGet, "/chats/prefs", nil))
	if rrNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("without a token: expected 401, got %d", rrNoAuth.Code)
	}
}

// TestIntegrationChatMuteCache — the cache that decides the fate of a push
// must agree with the database. A divergence here is invisible in every API
// response: the setting is "saved" and the notifications still arrive.
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
		t.Fatal("the cache did not update after mute — the push will go out anyway")
	}
	// Exactly one conversation is muted, not "everything incoming".
	if chatMuted(me, other) {
		t.Fatal("muting one conversation muted its neighbour")
	}
	// And for exactly one user.
	if chatMuted(peer, me) {
		t.Fatal("mute applied to the peer rather than to whoever set it")
	}

	// A server restart: the cache is rebuilt from the database.
	mutedMu.Lock()
	mutedSet = nil
	mutedMu.Unlock()
	loadMutedCache()
	if !chatMuted(me, peer) {
		t.Fatal("the mute setting was lost after a restart")
	}

	prefsPost(t, chatPrefsHandler, "/chats/prefs", tok, map[string]any{"peer": peer, "muted": false})
	if chatMuted(me, peer) {
		t.Fatal("unmute did not clear the cache entry")
	}
}

// TestIntegrationChatClear — a clear hides the correspondence from whoever
// cleared it and only from them; new messages are visible again afterwards.
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
		t.Fatalf("expected 3 messages before the clear, got %d", n)
	}

	rr := prefsPost(t, chatClearHandler, "/chats/clear", myTok, map[string]any{"peer": peer})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear: status %d: %s", rr.Code, rr.Body.String())
	}
	var cleared map[string]int64
	json.Unmarshal(rr.Body.Bytes(), &cleared)
	if cleared["cleared_seq"] == 0 {
		t.Fatal("the visibility boundary stayed at zero — the clear hid nothing")
	}

	if n := len(historyFor(t, myTok, peer)); n != 0 {
		t.Fatalf("%d messages are visible after the clear", n)
	}
	// The peer keeps the correspondence: "clear for me" is not "delete for all".
	if n := len(historyFor(t, peerTok, me)); n != 3 {
		t.Fatalf("the clear touched the peer's correspondence: %d of 3 left", n)
	}
	// And in the database too: messages must survive for any investigation.
	var inDB int
	db.QueryRow(`SELECT COUNT(*) FROM messages
	             WHERE (sender=$1 AND recipient=$2) OR (sender=$2 AND recipient=$1)`,
		me, peer).Scan(&inDB)
	if inDB != 3 {
		t.Fatalf("the clear deleted messages from the database: %d of 3 left", inDB)
	}

	// A new message gets a higher seq and is visible again: a clear is a
	// boundary in the past, not a block on the peer.
	if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("cc_msg"),
		From: peer, To: me, Body: "after the clear"}); err != nil {
		t.Fatal("saveMessage:", err)
	}
	h := historyFor(t, myTok, peer)
	if len(h) != 1 || h[0]["body"] != "after the clear" {
		t.Fatalf("expected one new message after the clear, got %v", h)
	}

	// A repeated clear must not move the boundary backwards.
	before := cleared["cleared_seq"]
	rr = prefsPost(t, chatClearHandler, "/chats/clear", myTok, map[string]any{"peer": peer})
	json.Unmarshal(rr.Body.Bytes(), &cleared)
	if cleared["cleared_seq"] < before {
		t.Fatalf("a repeated clear lowered the boundary: %d -> %d", before, cleared["cleared_seq"])
	}
}

// TestIntegrationChatClearOfflineQueue — a cleared conversation must not
// resurrect from the offline queue on the next connection. At the same time
// the rows must be marked delivered: otherwise they would stay pending
// forever, arrive on every reconnect, and the sender would always see
// "not delivered".
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
		t.Fatalf("expected 3 undelivered, got %d", got)
	}

	if rr := prefsPost(t, chatClearHandler, "/chats/clear", myTok,
		map[string]any{"peer": peer}); rr.Code != http.StatusOK {
		t.Fatalf("clear: status %d: %s", rr.Code, rr.Body.String())
	}

	c := testClient(me)
	deliverPending(me, c)

	if got := pendingCount(t, me); got != 0 {
		t.Fatalf("%d undelivered left after the clear — the queue would drain forever", got)
	}
	// Nothing went to the socket: the messages are hidden, not redelivered.
	select {
	case data := <-c.out:
		t.Fatalf("a cleared message still reached the client: %s", data)
	default:
	}
}

// TestIntegrationChatClearGroupScope — clearing a group for one member does
// not touch it for the others, and a foreign group cannot be cleared at all.
func TestIntegrationChatClearGroupScope(t *testing.T) {
	setupIntegration(t)
	owner, member, outsider := uniqueName("cg_own"), uniqueName("cg_mem"), uniqueName("cg_out")
	seedUsers(t, owner, member, outsider)
	gid := makeGroup(t, owner, member, uniqueName("Clear "))
	ownerTok, _ := generateToken(owner)
	memberTok, _ := generateToken(member)
	outsiderTok, _ := generateToken(outsider)

	for _, body := range []string{"one", "two"} {
		if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("cg_msg"),
			From: owner, To: gid, Body: body}); err != nil {
			t.Fatal("saveMessage:", err)
		}
	}

	// An outsider. For them "clear" is a request to remove a row from THEIR OWN
	// list, and it is harmless: answer with success but write no row. What is
	// checked is the absence of the row rather than the status code: a code is
	// a symptom, the property is what must hold. There used to be a 404 here,
	// and it told the asker whether a group with that id exists.
	if rr := prefsPost(t, chatClearHandler, "/chats/clear", outsiderTok,
		map[string]any{"peer": gid}); rr.Code != http.StatusOK {
		t.Fatalf("clear by an outsider: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var outsiderRows int
	db.QueryRow(`SELECT COUNT(*) FROM chat_prefs WHERE username=$1`, outsider).Scan(&outsiderRows)
	if outsiderRows != 0 {
		t.Fatalf("an outsider created %d chat_prefs rows — guessing ids writes to the table", outsiderRows)
	}
	// Muting a foreign group is still impossible: "muted" and "archived" only
	// make sense for a member, and writing such a row is exactly the
	// unbounded write being guarded against here.
	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", outsiderTok,
		map[string]any{"peer": gid, "muted": true}); rr.Code != http.StatusNotFound {
		t.Fatalf("an outsider muted a foreign group: expected 404, got %d", rr.Code)
	}

	if rr := prefsPost(t, chatClearHandler, "/chats/clear", memberTok,
		map[string]any{"peer": gid}); rr.Code != http.StatusOK {
		t.Fatalf("a member could not clear the group: status %d: %s", rr.Code, rr.Body.String())
	}
	if n := len(historyFor(t, memberTok, gid)); n != 0 {
		t.Fatalf("the member sees %d messages after the clear", n)
	}
	if n := len(historyFor(t, ownerTok, gid)); n != 2 {
		t.Fatalf("clearing for one member affected the others: the admin has %d of 2", n)
	}
}

// TestIntegrationChatClearVanishedPeer — a conversation with a deleted
// account must be deletable.
//
// ═══ WHAT WAS WRONG ═══════════════════════════════════════════════════════
//
// chatClearHandler answered 404 when the peer was not in users. It came out
// backwards: the one conversation a person CERTAINLY wants gone — with a
// dismissed and deleted employee — turned out to be the only one that could
// not be removed. The client said "check your connection" while the
// connection was fine and the server had answered quite definitely.
//
// No chat_prefs row is created, and none may be: there is nothing to hide
// (deleting an account erases the correspondence), and writing a row for any
// submitted name would turn the endpoint into an unbounded write by guessing.
func TestIntegrationChatClearVanishedPeer(t *testing.T) {
	setupIntegration(t)
	me, ghost := uniqueName("cv_me"), uniqueName("cv_ghost")
	seedUsers(t, me, ghost)
	myTok, _ := generateToken(me)

	if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("cv_msg"),
		From: ghost, To: me, Body: "before leaving"}); err != nil {
		t.Fatal("saveMessage:", err)
	}

	// The account is deleted the way the admin panel does it: the correspondence
	// first, then the user.
	db.Exec(`DELETE FROM messages WHERE sender=$1 OR recipient=$1`, ghost)
	db.Exec(`DELETE FROM users WHERE username=$1`, ghost)

	rr := prefsPost(t, chatClearHandler, "/chats/clear", myTok, map[string]any{"peer": ghost})
	if rr.Code != http.StatusOK {
		t.Fatalf("a conversation with a deleted user cannot be removed: status %d: %s", rr.Code, rr.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("the response does not parse as JSON: %s", rr.Body.String())
	}
	// The client needs the flag: it stops asking for that person's presence and
	// profile after seeing it (see web/js/helpers.js).
	if res["gone"] != true {
		t.Fatalf("the response carries no gone flag: %v", res)
	}

	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM chat_prefs WHERE username=$1 AND peer=$2`, me, ghost).Scan(&rows)
	if rows != 0 {
		t.Fatalf("a chat_prefs row was created for a non-existent peer (%d)", rows)
	}

	// The answer for a name that never existed must match the one for a deleted
	// account: otherwise the endpoint works as an oracle — the code would reveal
	// whether such a person exists.
	rr2 := prefsPost(t, chatClearHandler, "/chats/clear", myTok,
		map[string]any{"peer": uniqueName("cv_never")})
	if rr2.Code != rr.Code {
		t.Fatalf("deleted and never-existed are distinguishable by the answer: %d against %d",
			rr.Code, rr2.Code)
	}
}

// TestIntegrationDeleteUserClearsChatPrefs — deleting an account also removes
// other people's preferences for the conversation with it. Otherwise "muted"
// and "archived" set for that name would survive the deletion and be
// inherited, silently, by a new person if the name is created again.
func TestIntegrationDeleteUserClearsChatPrefs(t *testing.T) {
	setupIntegration(t)
	me, ghost := uniqueName("cp_me"), uniqueName("cp_ghost")
	seedUsers(t, me, ghost)
	myTok, _ := generateToken(me)

	if rr := prefsPost(t, chatPrefsHandler, "/chats/prefs", myTok,
		map[string]any{"peer": ghost, "muted": true, "archived": true}); rr.Code != http.StatusOK {
		t.Fatalf("setup: status %d: %s", rr.Code, rr.Body.String())
	}
	var before int
	db.QueryRow(`SELECT COUNT(*) FROM chat_prefs WHERE username=$1 AND peer=$2`, me, ghost).Scan(&before)
	if before != 1 {
		t.Fatalf("setup failed: %d chat_prefs rows, expected 1", before)
	}

	// Deleted exactly the way the panel does it — otherwise the test would check
	// its own SQL rather than the path accounts are actually deleted through.
	t.Setenv("ADMIN_KEY", "k")
	if cfg == nil {
		cfg = loadConfig()
	}
	delBody, _ := json.Marshal(map[string]string{"username": ghost, "action": "delete"})
	rrDel := httptest.NewRecorder()
	reqDel := httptest.NewRequest(http.MethodPost, "/admin/user-action", strings.NewReader(string(delBody)))
	reqDel.Header.Set("X-Admin-Key", "k")
	reqDel.RemoteAddr = "127.0.0.1:1"
	adminUserActionHandler(rrDel, reqDel)
	if rrDel.Code != http.StatusOK {
		t.Fatalf("account deletion: status %d: %s", rrDel.Code, rrDel.Body.String())
	}

	var after int
	db.QueryRow(`SELECT COUNT(*) FROM chat_prefs WHERE username=$1 OR peer=$1`, ghost).Scan(&after)
	if after != 0 {
		t.Fatalf("%d chat_prefs rows remain after the account was deleted", after)
	}
}
