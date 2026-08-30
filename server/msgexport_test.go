package main

// Message export (requires TEST_DATABASE_URL).
//
// Two kinds of failure are checked here, each expensive in its own way. The
// first: the export caught too much — a file destined for HR or a court
// contains other people's correspondence. The second: the export lost what
// mattered — a decision is made from an incomplete file as if it were whole.
//
// Separately: an export cannot be performed unnoticed — it is not done without
// a reason, and the reason lands in the administrator's log.

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var msgExportOnce sync.Once

func setupMsgExport(t *testing.T) {
	t.Helper()
	setupIntegration(t)
	msgExportOnce.Do(initAdminSchema)

	// adminGuard sets CORS from cfg and checks the key.
	origCfg, origKey, origIPs := cfg, adminKey, adminAllowedIPs
	if cfg == nil {
		cfg = loadConfig()
	}
	adminKey = func() string { return "test-admin-key" }
	adminAllowedIPs = map[string]bool{}
	t.Cleanup(func() { cfg, adminKey, adminAllowedIPs = origCfg, origKey, origIPs })
}

// putMessage inserts a message directly, bypassing the writer queue: the test
// needs control over created_at, while saveMessage stamps the current time.
func putMessage(t *testing.T, id, sender, recipient, body string, at time.Time, deleted bool) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO messages(id, sender, recipient, body, deleted, created_at) VALUES($1,$2,$3,$4,$5,$6)`,
		id, sender, recipient, encryptBody(body), deleted, at)
	if err != nil {
		t.Fatalf("inserting message %s: %v", id, err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM messages WHERE id=$1`, id) })
}

func exportRequest(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/message-export?"+query, nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	req.RemoteAddr = "198.51.100.7:1234"
	adminMessageExportHandler(rr, req)
	return rr
}

// exportJSON exports as JSON and parses the response.
func exportJSON(t *testing.T, query string) map[string]any {
	t.Helper()
	rr := exportRequest(t, query+"&format=json")
	if rr.Code != http.StatusOK {
		t.Fatalf("the export returned %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	return out
}

func exportBodies(t *testing.T, query string) []string {
	t.Helper()
	msgs, _ := exportJSON(t, query)["messages"].([]any)
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		row, _ := m.(map[string]any)
		s, _ := row["body"].(string)
		out = append(out, s)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ── Mandatory parameters ──────────────────────────────────────────────────

// There must be no "export everything" button: a missing parameter has no
// right to turn into a dump of the whole company's correspondence.
func TestMessageExportRequiresUserAndReason(t *testing.T) {
	setupMsgExport(t)

	for _, q := range []string{
		"reason=internal+investigation",       // no user
		"user=&reason=internal+investigation", // empty user
		"user=bad+name!&reason=internal+inv",  // not a name
		"user=someone",                        // no reason
		"user=someone&reason=x",               // a token reason
		"user=someone&reason=+++++++++++",     // spaces are not a reason
	} {
		if code := exportRequest(t, q).Code; code != http.StatusBadRequest {
			t.Errorf("%q: expected 400, got %d", q, code)
		}
	}

	// A control: with both parameters the export succeeds. Without it the test
	// would pass on "always refuse" as well.
	if code := exportRequest(t, "user=someone&reason=internal+investigation").Code; code != http.StatusOK {
		t.Fatalf("a correct request was rejected: %d", code)
	}
}

// THE MAIN POINT: no one else's correspondence may enter the export.
func TestMessageExportDoesNotLeakOtherPeoplesChats(t *testing.T) {
	setupMsgExport(t)
	subject, peer, outsider := uniqueName("ex_s"), uniqueName("ex_p"), uniqueName("ex_o")
	now := time.Now()

	putMessage(t, uniqueName("m"), subject, peer, "mine outgoing", now, false)
	putMessage(t, uniqueName("m"), peer, subject, "mine incoming", now, false)
	putMessage(t, uniqueName("m"), peer, outsider, "none of my business", now, false)
	putMessage(t, uniqueName("m"), outsider, peer, "also not mine", now, false)

	got := exportBodies(t, "user="+subject+"&reason=internal+investigation")
	for _, leaked := range []string{"none of my business", "also not mine"} {
		if contains(got, leaked) {
			t.Errorf("SOMEONE ELSE'S correspondence entered the export: %q", leaked)
		}
	}
	for _, want := range []string{"mine outgoing", "mine incoming"} {
		if !contains(got, want) {
			t.Errorf("the export is missing the user's own message %q", want)
		}
	}
}

// Bodies in the database are encrypted — the export must return readable text,
// otherwise the endpoint loses its whole purpose.
func TestMessageExportDecryptsBodies(t *testing.T) {
	setupMsgExport(t)
	a, b := uniqueName("ex_a"), uniqueName("ex_b")
	secret := "quarterly report, draft 🚀"
	putMessage(t, uniqueName("m"), a, b, secret, time.Now(), false)

	if !contains(exportBodies(t, "user="+a+"&reason=internal+investigation"), secret) {
		t.Fatal("the body was not decrypted — the file would contain base64 rubbish")
	}
}

// Group messages: a member sees the whole group, an outsider sees nothing.
func TestMessageExportIncludesGroupsOfMember(t *testing.T) {
	setupMsgExport(t)
	member, other, outsider := uniqueName("ex_m"), uniqueName("ex_t"), uniqueName("ex_x")
	gid := "g:" + uniqueName("grp")[:12]

	if _, err := db.Exec(`INSERT INTO groups(id, name, created_by) VALUES($1,$2,$3)`,
		gid, "Quarterly", member); err != nil {
		t.Fatalf("creating a group: %v", err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM group_members WHERE group_id=$1`, gid)
		db.Exec(`DELETE FROM groups WHERE id=$1`, gid)
	})
	for _, u := range []string{member, other} {
		if _, err := db.Exec(`INSERT INTO group_members(group_id, username) VALUES($1,$2)`, gid, u); err != nil {
			t.Fatalf("adding to a group: %v", err)
		}
	}
	putMessage(t, uniqueName("m"), other, gid, "group message from a colleague", time.Now(), false)

	got := exportBodies(t, "user="+member+"&reason=internal+investigation")
	if !contains(got, "group message from a colleague") {
		t.Error("a message from the member's group did not reach the export")
	}

	// The outsider is not in that group — for them it does not exist.
	if contains(exportBodies(t, "user="+outsider+"&reason=internal+investigation"),
		"group message from a colleague") {
		t.Error("an outsider's export contains a group they do not belong to")
	}

	// The group name is substituted — a report of "g:ab12cd34" is unreadable.
	msgs, _ := exportJSON(t, "user="+member+"&reason=internal+investigation")["messages"].([]any)
	found := false
	for _, m := range msgs {
		row, _ := m.(map[string]any)
		if row["recipient"] == gid {
			found = true
			if chat, _ := row["chat"].(string); !strings.Contains(chat, "Quarterly") {
				t.Errorf("the group name was not substituted: %q", chat)
			}
			if row["chat_kind"] != "group" {
				t.Errorf("a group message is marked as %v", row["chat_kind"])
			}
		}
	}
	if !found {
		t.Error("the group message was not found in the export")
	}
}

// Deleted messages: absent by default, present and marked on request.
func TestMessageExportDeletedOptInAndFlagged(t *testing.T) {
	setupMsgExport(t)
	a, b := uniqueName("ex_da"), uniqueName("ex_db")
	putMessage(t, uniqueName("m"), a, b, "kept", time.Now(), false)
	putMessage(t, uniqueName("m"), a, b, "removed by the author", time.Now(), true)

	base := "user=" + a + "&reason=internal+investigation"
	if contains(exportBodies(t, base), "removed by the author") {
		t.Error("a deleted message entered an ordinary export")
	}

	msgs, _ := exportJSON(t, base+"&deleted=1")["messages"].([]any)
	var sawDeleted bool
	for _, m := range msgs {
		row, _ := m.(map[string]any)
		if row["body"] == "removed by the author" {
			sawDeleted = true
			if row["deleted"] != true {
				t.Error("a deleted message is not marked — it would be presented as ordinary")
			}
		}
	}
	if !sawDeleted {
		t.Error("with deleted=1 the deleted message was not exported")
	}
}

// Narrowing to one peer is the typical "correspondence of X with Y" request.
func TestMessageExportNarrowsToPeer(t *testing.T) {
	setupMsgExport(t)
	a, b, c := uniqueName("ex_na"), uniqueName("ex_nb"), uniqueName("ex_nc")
	putMessage(t, uniqueName("m"), a, b, "talking to b", time.Now(), false)
	putMessage(t, uniqueName("m"), a, c, "talking to c", time.Now(), false)

	got := exportBodies(t, "user="+a+"&with="+b+"&reason=internal+investigation")
	if !contains(got, "talking to b") {
		t.Error("the requested conversation was not exported")
	}
	if contains(got, "talking to c") {
		t.Error("narrowing by peer did not work — an unrelated conversation got in")
	}
}

// The period: nothing outside its bounds is taken.
func TestMessageExportRespectsPeriod(t *testing.T) {
	setupMsgExport(t)
	a, b := uniqueName("ex_pa"), uniqueName("ex_pb")
	inside := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	before := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	putMessage(t, uniqueName("m"), a, b, "inside the window", inside, false)
	putMessage(t, uniqueName("m"), a, b, "before the window", before, false)
	putMessage(t, uniqueName("m"), a, b, "after the window", after, false)

	got := exportBodies(t, "user="+a+"&from=2026-03-01&to=2026-03-31&reason=internal+investigation")
	if !contains(got, "inside the window") {
		t.Error("a message inside the period was lost")
	}
	for _, out := range []string{"before the window", "after the window"} {
		if contains(got, out) {
			t.Errorf("%q was exported from outside the period", out)
		}
	}

	// The last day of the period is included in full: otherwise it would be lost
	// systematically, and only an incomplete report would reveal it.
	edge := time.Date(2026, 3, 31, 23, 30, 0, 0, time.UTC)
	putMessage(t, uniqueName("m"), a, b, "late on the last day", edge, false)
	if !contains(exportBodies(t, "user="+a+"&from=2026-03-01&to=2026-03-31&reason=internal+investigation"),
		"late on the last day") {
		t.Error("the evening of the last day did not reach the export")
	}
}

// Attachments: without file names the export is incomplete — the body refers
// to a file with a random name that would have to be found by hand.
func TestMessageExportListsAttachments(t *testing.T) {
	setupMsgExport(t)
	a, b := uniqueName("ex_fa"), uniqueName("ex_fb")
	putMessage(t, uniqueName("m"), a, b, "see /files/abc123.png and /files/abc123.png", time.Now(), false)

	msgs, _ := exportJSON(t, "user="+a+"&reason=internal+investigation")["messages"].([]any)
	var got string
	for _, m := range msgs {
		row, _ := m.(map[string]any)
		if s, _ := row["attachments"].(string); s != "" {
			got = s
		}
	}
	if got != "abc123.png" {
		t.Fatalf("attachment names %q, expected a single \"abc123.png\" (no duplicates)", got)
	}
}

// Exporting someone else's correspondence cannot be done unnoticed.
func TestMessageExportIsAudited(t *testing.T) {
	setupMsgExport(t)
	subject := uniqueName("ex_au")
	reason := "HR case 2026-114"

	if code := exportRequest(t, "user="+subject+"&reason="+strings.ReplaceAll(reason, " ", "+")).Code; code != http.StatusOK {
		t.Fatalf("the export failed: %d", code)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM admin_audit WHERE target=$1`, subject) })

	var details string
	err := db.QueryRow(
		`SELECT COALESCE(details,'') FROM admin_audit WHERE action='message_export' AND target=$1
		 ORDER BY created_at DESC LIMIT 1`, subject).Scan(&details)
	if err != nil {
		t.Fatalf("there is no export record in the administrator's log: %v", err)
	}
	if !strings.Contains(details, reason) {
		t.Errorf("the export reason did not reach the log: %q", details)
	}
}

// CSV: a BOM for Excel and a full header row. Without the BOM, non-ASCII text
// turns to rubbish for whoever opens the report.
func TestMessageExportCSVShape(t *testing.T) {
	setupMsgExport(t)
	a, b := uniqueName("ex_ca"), uniqueName("ex_cb")
	putMessage(t, uniqueName("m"), a, b, "a line with non-ASCII: Gruesse, 日本", time.Now(), false)

	rr := exportRequest(t, "user="+a+"&reason=internal+investigation")
	if rr.Code != http.StatusOK {
		t.Fatalf("CSV returned %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.HasPrefix(body, "\xEF\xBB\xBF") {
		t.Error("no UTF-8 BOM — Excel will read the text in the system code page")
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, a) {
		t.Errorf("the file name does not name the employee: %q", cd)
	}
	// Someone else's correspondence must not settle in an intermediate cache.
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, expected no-store", cc)
	}

	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("the CSV does not parse: %v", err)
	}
	if len(recs) < 2 {
		t.Fatalf("the CSV has no data rows: %v", recs)
	}
	for _, col := range []string{"at_utc", "chat", "direction", "body", "attachments", "deleted"} {
		if !contains(recs[0], col) {
			t.Errorf("the CSV header has no %q column", col)
		}
	}
	if !contains(recs[1], "a line with non-ASCII: Gruesse, 日本") {
		t.Errorf("the message body is not in the data row: %v", recs[1])
	}
}
