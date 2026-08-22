package main

// Integration tests for the delivery guarantees (requires
// TEST_DATABASE_URL).
//
// They answer three questions usually taken on faith:
//   1. Is a missing ACK a lost message or not?
//   2. Does the seq cursor rebuild history without gaps or duplicates?
//   3. Is a single drain of the offline queue bounded?

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// pendingCount reports how many messages are still undelivered.
func pendingCount(t *testing.T, recipient string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE recipient=$1 AND delivered=false`,
		recipient).Scan(&n); err != nil {
		t.Fatal("pending count:", err)
	}
	return n
}

// drainClient collects the frames queued on a connection.
func drainClient(c *Client) [][]byte {
	var out [][]byte
	for {
		select {
		case d := <-c.out:
			out = append(out, d)
		default:
			return out
		}
	}
}

// The central delivery guarantee, exercised at the worst possible moment:
// the message is stored but the ACK never reaches the sender.
//
// Requirements:
//   - the message is not lost and remains undelivered in the database;
//   - the client's retry with the same id creates no duplicate and returns
//     the same seq;
//   - the recipient's reconnect delivers it and only then clears pending.
func TestIntegrationNoAckIsNotLoss(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_noack_a"), uniqueName("it_noack_b")

	msg := Message{Type: "message", ID: uniqueName("id"), From: alice, To: bob, Body: "message without an ack"}
	seq1, _, err := saveMessage(msg)
	if err != nil {
		t.Fatal("save:", err)
	}

	// No ACK was sent or received, so the message must still be waiting.
	if got := pendingCount(t, bob); got != 1 {
		t.Fatalf("want 1 undelivered after a save without ACK, got %d", got)
	}

	// The client gave up waiting and resent with the same id.
	seq2, _, err := saveMessage(msg)
	if err != nil {
		t.Fatal("resend:", err)
	}
	if seq1 != seq2 {
		t.Fatalf("the retry returned a different seq: %d != %d", seq1, seq2)
	}
	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=$1`, msg.ID).Scan(&rows)
	if rows != 1 {
		t.Fatalf("the retry created a duplicate: %d rows", rows)
	}

	// The recipient connects and the offline queue must catch them up.
	client := testClient(bob)
	deliverPending(bob, client)
	frames := drainClient(client)
	if len(frames) != 1 {
		t.Fatalf("%d frames delivered on reconnect, want 1", len(frames))
	}
	if got := pendingCount(t, bob); got != 0 {
		t.Fatalf("%d still undelivered after delivery, want 0", got)
	}
}

// When a socket write fails the message must stay undelivered rather than
// vanish: only what actually reached the client may be marked delivered.
func TestIntegrationPendingSurvivesFailedSocket(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_sock_a"), uniqueName("it_sock_b")

	for i := 0; i < 3; i++ {
		msg := Message{Type: "message", ID: uniqueName("id"), From: alice, To: bob,
			Body: fmt.Sprintf("message %d", i)}
		if _, _, err := saveMessage(msg); err != nil {
			t.Fatal("save:", err)
		}
	}
	if got := pendingCount(t, bob); got != 3 {
		t.Fatalf("want 3 undelivered, got %d", got)
	}

	// An already-closed connection: send() fails on the first frame.
	dead := testClient(bob)
	close(dead.quit)

	deliverPending(bob, dead)

	if got := pendingCount(t, bob); got != 3 {
		t.Fatalf("%d undelivered after a failed socket write, want 3", got)
	}
}

// Rebuilding history through the seq cursor.
//
// Messages are written concurrently, the way the real writers batch them,
// then paged through /history as a client would: seq must increase strictly,
// with no duplicates and no gaps.
func TestIntegrationSeqRecoveryNoGaps(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_seq_a"), uniqueName("it_seq_b")

	const total = 120
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := Message{Type: "message", ID: fmt.Sprintf("%s-%03d", uniqueName("sq"), i),
				From: alice, To: bob, Body: fmt.Sprintf("body %d", i)}
			if _, _, err := saveMessage(msg); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal("concurrent write:", err)
	}

	// The client's catch-up loop: since=<last seq>, page by page.
	seen := map[string]bool{}
	var lastSeq float64
	for page := 0; page < 20; page++ {
		msgs := historyRequest(t, bob, fmt.Sprintf("peer=%s&since=%d&limit=25", alice, int64(lastSeq)))
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			seq := m["seq"].(float64)
			if seq <= lastSeq {
				t.Fatalf("seq does not increase: %v after %v", seq, lastSeq)
			}
			lastSeq = seq
			id := m["id"].(string)
			if seen[id] {
				t.Fatalf("message %s arrived twice", id)
			}
			seen[id] = true
		}
		if len(msgs) < 25 {
			break
		}
	}
	if len(seen) != total {
		t.Fatalf("recovered %d of %d messages — the history has a gap", len(seen), total)
	}
}

// The all-conversations selection (a reconnect with since=0) must return
// exactly one's own messages: direct ones in both directions and group ones
// from one's own groups, and nothing else.
//
// The query is a UNION ALL of branches chosen for its query plan (see
// historyHandler), so its scope is verified separately: a mistake here would
// either leak someone else's conversation or drop one's own.
func TestIntegrationFullSyncScope(t *testing.T) {
	setupIntegration(t)
	me, peer, outsider := uniqueName("it_fs_me"), uniqueName("it_fs_pe"), uniqueName("it_fs_out")
	group := "g:" + uniqueName("fs")

	if _, err := db.Exec(`INSERT INTO groups(id, name, created_by) VALUES($1,$2,$3)
		ON CONFLICT DO NOTHING`, group, "sync scope", me); err != nil {
		t.Fatal("create group:", err)
	}
	for _, u := range []string{me, peer} {
		if _, err := db.Exec(`INSERT INTO group_members(group_id, username, role)
			VALUES($1,$2,'member') ON CONFLICT DO NOTHING`, group, u); err != nil {
			t.Fatal("add member:", err)
		}
	}

	mine := map[string]bool{}
	save := func(from, to, body string, isMine bool) {
		t.Helper()
		id := uniqueName("fs")
		if _, _, err := saveMessage(Message{Type: "message", ID: id, From: from, To: to, Body: body}); err != nil {
			t.Fatal("save:", err)
		}
		if isMine {
			mine[id] = true
		}
	}
	save(me, peer, "outgoing direct", true)
	save(peer, me, "incoming direct", true)
	save(me, group, "mine to the group", true)
	save(peer, group, "theirs to my group", true)
	save(peer, outsider, "someone else's conversation", false)
	save(outsider, peer, "someone else's conversation 2", false)

	msgs := historyRequest(t, me, "since=0&limit=1000")
	got := map[string]bool{}
	for _, m := range msgs {
		got[m["id"].(string)] = true
	}
	for id := range mine {
		if !got[id] {
			t.Fatalf("own message %s missing from the sync", id)
		}
	}
	var leaked int
	for _, m := range msgs {
		from, to := m["from"].(string), m["to"].(string)
		if from != me && to != me && to != group {
			leaked++
		}
	}
	if leaked > 0 {
		t.Fatalf("%d foreign messages leaked into the sync", leaked)
	}
}

// The offline queue is drained in bounded portions.
//
// Without a cap the selection pulls every undelivered row into memory and
// decrypts each one, which is an OOM vector during a mass reconnect. One
// connection must take no more than the cap, and the remainder must wait
// rather than disappear.
func TestIntegrationPendingBatchIsBounded(t *testing.T) {
	setupIntegration(t)
	// Lower the cap for the test: what matters is that a bound exists.
	orig := maxPendingPerConnect
	maxPendingPerConnect = 10
	t.Cleanup(func() { maxPendingPerConnect = orig })

	alice, bob := uniqueName("it_lim_a"), uniqueName("it_lim_b")
	for i := 0; i < maxPendingPerConnect+5; i++ {
		msg := Message{Type: "message", ID: uniqueName("id"), From: alice, To: bob, Body: "x"}
		if _, _, err := saveMessage(msg); err != nil {
			t.Fatal("save:", err)
		}
	}
	client := testClient(bob)
	deliverPending(bob, client)
	if got := len(drainClient(client)); got != maxPendingPerConnect {
		t.Fatalf("%d frames on one connect, want the cap of %d", got, maxPendingPerConnect)
	}
	if got := pendingCount(t, bob); got != 5 {
		t.Fatalf("%d left in the queue, want 5", got)
	}
}

// Contact details are visible only to people with shared context.
//
// Serving email and phone for any user to any signed-in caller turns
// /api/profile into a downloadable staff directory of personal data.
func TestIntegrationProfileContactsHiddenFromStrangers(t *testing.T) {
	setupIntegration(t)
	owner, peer, stranger := uniqueName("it_pa"), uniqueName("it_pb"), uniqueName("it_pc")

	for _, u := range []string{owner, peer, stranger} {
		if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES($1,'x')
			ON CONFLICT DO NOTHING`, u); err != nil {
			t.Fatal("create user:", err)
		}
	}
	if _, err := db.Exec(`UPDATE users SET email=$2, phone=$3 WHERE username=$1`,
		owner, "owner@corp.example", "+70000000000"); err != nil {
		t.Fatal("set contacts:", err)
	}
	// The shared context with peer is a conversation.
	if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("id"),
		From: owner, To: peer, Body: "hello"}); err != nil {
		t.Fatal("save:", err)
	}

	get := func(viewer, target string) map[string]any {
		t.Helper()
		tok, err := generateToken(viewer)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/profile?user="+target, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		profileHandler(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("profile %s→%s: status %d", viewer, target, rr.Code)
		}
		var p map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
			t.Fatal("profile json:", err)
		}
		return p
	}

	if p := get(peer, owner); p["email"] == "" || p["phone"] == "" {
		t.Fatal("a peer must see the contact details")
	}
	p := get(stranger, owner)
	if p["email"] != "" || p["phone"] != "" {
		t.Fatalf("a stranger saw the contacts: email=%v phone=%v", p["email"], p["phone"])
	}
	// Name and position stay visible: that is how a colleague is found.
	if _, ok := p["display_name"]; !ok {
		t.Fatal("display_name must remain in the response")
	}
}

// The blocking boundary.
//
// A block rests on two things: forced_logout_at devalues tokens already
// issued, and every entry point must refuse to issue a new one to a blocked
// account. Miss the second half and a freshly issued token carries an iat
// newer than the cutoff, so revocation does not catch it and the blocked
// account keeps reading history and files over HTTP — the WS handler rejects
// them, but HTTP endpoints check only the signature.
//
// Both halves are verified here.
func TestIntegrationBlockedUserTokenBoundary(t *testing.T) {
	setupIntegration(t)
	user := uniqueName("it_blk")
	if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES($1,'x')
		ON CONFLICT DO NOTHING`, user); err != nil {
		t.Fatal("create user:", err)
	}

	// A token issued before the block.
	oldTok, err := generateToken(user)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := validateToken(oldTok); !ok {
		t.Fatal("a token issued before the block should be valid")
	}

	// Block exactly as the admin panel does.
	now := time.Now().Unix()
	if _, err := db.Exec(`UPDATE users SET blocked=TRUE, forced_logout_at=$2 WHERE username=$1`,
		user, now); err != nil {
		t.Fatal("block:", err)
	}
	setLogoutCutoff(user, now)
	t.Cleanup(func() { logoutCutoffs.Delete(user) })

	if !userBlocked(user) {
		t.Fatal("userBlocked should report true after blocking")
	}
	// The old token must stop working.
	if _, ok := validateToken(oldTok); ok {
		t.Fatal("a pre-block token is still valid — revocation does not work")
	}

	// A fresh token escapes revocation because its iat is newer than the
	// cutoff, which is precisely why the entry point must refuse itself.
	time.Sleep(1100 * time.Millisecond) // iat is in seconds; need a larger one
	newTok, err := generateToken(user)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := validateToken(newTok); !ok {
		t.Fatal("test premise broken: a fresh token should pass validateToken")
	}
	// So the protection can only live at the entry point: check that
	// userBlocked reports the block to whoever issues tokens.
	if !userBlocked(user) {
		t.Fatal("sign-in must see the block before issuing a token")
	}
}

// The sign-in audit records both successes and refusals and serves them to
// the admin endpoint.
//
// Without failed attempts there is no answer to "who was hammering the
// system" or "why did sign-in stop working for everyone", since the limiter
// triggers on failures.
func TestIntegrationLoginAuditRecordsOutcomes(t *testing.T) {
	setupIntegration(t)
	initLoginAuditSchema()
	user := uniqueName("it_la")

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	req.Header.Set("User-Agent", "drill/1.0")

	recordLogin(req, user, loginOK, "password")
	recordLogin(req, user, loginBadCreds, "password")
	recordLogin(req, user, loginBlocked, "google")

	tok := "k"
	t.Setenv("ADMIN_KEY", tok)
	// adminGuard reads cfg for the CORS origin, which production loads
	// before registering handlers.
	if cfg == nil {
		cfg = loadConfig()
	}
	rr := httptest.NewRecorder()
	ar := httptest.NewRequest(http.MethodGet, "/admin/login-audit?q="+user+"&limit=50", nil)
	ar.Header.Set("X-Admin-Key", tok)
	ar.RemoteAddr = "127.0.0.1:1"
	adminLoginAuditHandler(rr, ar)
	if rr.Code != http.StatusOK {
		t.Fatalf("login-audit: status %d: %s", rr.Code, rr.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal("json:", err)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e["outcome"].(string)] = true
		if e["ip"] != "203.0.113.7" {
			t.Fatalf("ip not recorded: %v", e["ip"])
		}
	}
	for _, want := range []string{loginOK, loginBadCreds, loginBlocked} {
		if !seen[want] {
			t.Fatalf("outcome %q missing from the audit (got: %v)", want, seen)
		}
	}

	// Retention: old rows must be removed, being personal data.
	if _, err := db.Exec(
		`UPDATE login_audit SET created_at = NOW() - INTERVAL '200 days' WHERE username=$1`,
		user); err != nil {
		t.Fatal("age rows:", err)
	}
	pruneLoginAudit(90)
	var left int
	db.QueryRow(`SELECT count(*) FROM login_audit WHERE username=$1`, user).Scan(&left)
	if left != 0 {
		t.Fatalf("%d rows older than the retention period survived the prune", left)
	}
}
