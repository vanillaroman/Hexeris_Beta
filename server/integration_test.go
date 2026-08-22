package main

// Integration tests against a real Postgres, covering what unit tests cannot:
// saveMessage idempotency, end-to-end /history with decryption, edit/delete
// authorisation and read receipts.
//
// They run only when TEST_DATABASE_URL is set:
//   locally:   docker run --rm -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=hexeris_test \
//              -p 55432:5432 postgres:16
//              TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/hexeris_test?sslmode=disable' \
//              go test -run Integration ./...
//   CI:        a postgres service container (.github/workflows/ci.yml).
// Without it every test skips, so a plain `go test ./...` still needs
// nothing but Go.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

var integrationInit sync.Once

func setupIntegration(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	stubKeys(t)
	integrationInit.Do(func() {
		os.Setenv("DATABASE_URL", dsn)
		initDB() // the same idempotent schema as production
		// saveMessage enqueues and waits for a writer, so without the
		// writers running the call would hang forever.
		startMessageWriters()
	})
}

// Per-run names, so repeated runs against one database stay independent.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s%d%04d", prefix, rand.Intn(1000000), rand.Intn(10000))
}

func TestIntegrationSaveMessageIdempotent(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_a"), uniqueName("it_b")
	msg := Message{Type: "message", ID: uniqueName("id"), From: alice, To: bob, Body: "hello, world"}

	seq1, ts1, err := saveMessage(msg)
	if err != nil {
		t.Fatal("first save:", err)
	}
	// Re-sending the same id — a client retry after a dropped socket — must
	// return the same seq and create no second row; guaranteed delivery
	// rests on this.
	seq2, ts2, err := saveMessage(msg)
	if err != nil {
		t.Fatal("second save:", err)
	}
	if seq1 != seq2 || ts1 != ts2 {
		t.Fatalf("resend not idempotent: seq %d != %d or ts %d != %d", seq1, seq2, ts1, ts2)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=$1`, msg.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 row, got %d", n)
	}
	// The stored body is ciphertext, not plaintext.
	var stored string
	db.QueryRow(`SELECT body FROM messages WHERE id=$1`, msg.ID).Scan(&stored)
	if stored == msg.Body {
		t.Fatal("body stored as plaintext")
	}
	if got := decryptBody(stored); got != msg.Body {
		t.Fatalf("decrypt roundtrip: %q != %q", got, msg.Body)
	}
}

func historyRequest(t *testing.T, user, query string) []map[string]any {
	t.Helper()
	tok, err := generateToken(user)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/history?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	historyHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("history %q: status %d: %s", query, rr.Code, rr.Body.String())
	}
	var msgs []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("history json: %v", err)
	}
	return msgs
}

func TestIntegrationHistoryFlow(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_a"), uniqueName("it_b")

	bodies := []string{"first", "second", "third"}
	for _, b := range bodies {
		if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("id"), From: alice, To: bob, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	// Media messages store the URL as-is.
	mediaID := uniqueName("id")
	if _, _, err := saveMessage(Message{Type: "message", ID: mediaID, From: alice, To: bob, Body: "/files/x.png", MediaType: "image"}); err != nil {
		t.Fatal(err)
	}

	msgs := historyRequest(t, alice, "peer="+bob+"&since=0")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	// Ascending by seq, with bodies decrypted by the server.
	for i, want := range bodies {
		if msgs[i]["body"] != want {
			t.Fatalf("msg[%d] body %q, want %q (decryption or order broken)", i, msgs[i]["body"], want)
		}
	}
	if msgs[3]["body"] != "/files/x.png" || msgs[3]["media_type"] != "image" {
		t.Fatalf("media message mangled: %v", msgs[3])
	}

	// Paging backwards: before=<seq> returns only older messages.
	lastSeq := int64(msgs[3]["seq"].(float64))
	older := historyRequest(t, alice, fmt.Sprintf("peer=%s&before=%d", bob, lastSeq))
	if len(older) != 3 {
		t.Fatalf("before-pagination: expected 3, got %d", len(older))
	}
	for _, m := range older {
		if int64(m["seq"].(float64)) >= lastSeq {
			t.Fatalf("before-pagination returned seq >= cursor: %v", m["seq"])
		}
	}
}

func TestIntegrationGroupHistoryMembersOnly(t *testing.T) {
	setupIntegration(t)
	alice, mallory := uniqueName("it_a"), uniqueName("it_m")
	gid := "g:" + uniqueName("grp")
	if _, err := db.Exec(`INSERT INTO groups(id, name, created_by) VALUES($1,'Test',$2)`, gid, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO group_members(group_id, username, role) VALUES($1,$2,'owner')`, gid, alice); err != nil {
		t.Fatal(err)
	}

	tok, _ := generateToken(mallory)
	req := httptest.NewRequest(http.MethodGet, "/history?peer="+gid, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	historyHandler(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-member got group history: status %d", rr.Code)
	}
}

func postJSON(t *testing.T, handler http.HandlerFunc, user, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	tok, err := generateToken(user)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func TestIntegrationEditDeleteAuthz(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_a"), uniqueName("it_b")
	msgID := uniqueName("id")
	if _, _, err := saveMessage(Message{Type: "message", ID: msgID, From: alice, To: bob, Body: "original"}); err != nil {
		t.Fatal(err)
	}

	// Someone else's message cannot be edited.
	if rr := postJSON(t, editMessageHandler, bob, "/edit-message",
		map[string]string{"msg_id": msgID, "body": "hijacked"}); rr.Code != http.StatusForbidden {
		t.Fatalf("non-owner edit: status %d, want 403", rr.Code)
	}
	// The length limit applies to edits too, which would otherwise be a
	// way around it.
	if rr := postJSON(t, editMessageHandler, alice, "/edit-message",
		map[string]string{"msg_id": msgID, "body": strings.Repeat("x", 5000)}); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized edit: status %d, want 413", rr.Code)
	}
	// Own message within the limit succeeds, and is stored encrypted.
	if rr := postJSON(t, editMessageHandler, alice, "/edit-message",
		map[string]string{"msg_id": msgID, "body": "corrected"}); rr.Code != http.StatusOK {
		t.Fatalf("owner edit: status %d: %s", rr.Code, rr.Body.String())
	}
	var stored string
	var edited bool
	db.QueryRow(`SELECT body, edited FROM messages WHERE id=$1`, msgID).Scan(&stored, &edited)
	if !edited || decryptBody(stored) != "corrected" || stored == "corrected" {
		t.Fatalf("edit not persisted encrypted: edited=%v body=%q", edited, stored)
	}

	// Deletion: someone else's is 403, one's own is marked deleted and its
	// body hidden.
	if rr := postJSON(t, deleteMessageHandler, bob, "/delete-message",
		map[string]string{"msg_id": msgID}); rr.Code != http.StatusForbidden {
		t.Fatalf("non-owner delete: status %d, want 403", rr.Code)
	}
	if rr := postJSON(t, deleteMessageHandler, alice, "/delete-message",
		map[string]string{"msg_id": msgID}); rr.Code != http.StatusOK {
		t.Fatalf("owner delete: status %d", rr.Code)
	}
	msgs := historyRequest(t, alice, "peer="+bob+"&since=0")
	last := msgs[len(msgs)-1]
	if last["deleted"] != true || last["body"] != "[deleted]" {
		t.Fatalf("deleted message leaks body: %v", last)
	}
}

func TestIntegrationRetention(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_a"), uniqueName("it_b")

	oldID, newID := uniqueName("id"), uniqueName("id")
	for _, id := range []string{oldID, newID} {
		if _, _, err := saveMessage(Message{Type: "message", ID: id, From: alice, To: bob, Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	// Age one message and react to it: the reaction must go with it, since
	// reactions have no foreign key.
	if _, err := db.Exec(`UPDATE messages SET created_at = NOW() - INTERVAL '400 days' WHERE id=$1`, oldID); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO reactions(msg_id, username, emoji) VALUES($1,$2,'👍')`, oldID, bob)

	if msgs, _, _ := runRetentionOnce(365, 0); msgs < 1 {
		t.Fatalf("retention deleted %d messages, expected >= 1", msgs)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=$1`, oldID).Scan(&n)
	if n != 0 {
		t.Fatal("old message survived retention")
	}
	db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE msg_id=$1`, oldID).Scan(&n)
	if n != 0 {
		t.Fatal("reaction outlived its message")
	}
	// The recent message is untouched.
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id=$1`, newID).Scan(&n)
	if n != 1 {
		t.Fatal("retention deleted a recent message")
	}
	// msgDays=0 means messages are never touched.
	if msgs, _, _ := runRetentionOnce(0, 0); msgs != 0 {
		t.Fatalf("msgDays=0 must be a no-op, deleted %d", msgs)
	}
}

func TestIntegrationReadReceipts(t *testing.T) {
	setupIntegration(t)
	alice, bob := uniqueName("it_a"), uniqueName("it_b")
	for i := 0; i < 2; i++ {
		if _, _, err := saveMessage(Message{Type: "message", ID: uniqueName("id"), From: alice, To: bob, Body: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	// bob read the conversation with alice (From is the reader, To the
	// original sender).
	handleRead(Message{Type: "read", From: bob, To: alice})
	var unread int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE sender=$1 AND recipient=$2 AND read=false`, alice, bob).Scan(&unread)
	if unread != 0 {
		t.Fatalf("%d messages still unread after handleRead", unread)
	}
}
