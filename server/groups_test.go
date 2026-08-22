package main

// Group management: renaming, description and disbanding (requires
// TEST_DATABASE_URL).
//
// Beyond the happy path these cover the boundary of authority: a group admin
// is not a server admin, and their powers end at their own group.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// groupPost calls a group endpoint as the token's owner.
func groupPost(t *testing.T, h http.HandlerFunc, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "127.0.0.1:1"
	h(rr, req)
	return rr
}

// makeGroup creates a group with owner as admin and member as a member.
func makeGroup(t *testing.T, owner, member, name string) string {
	t.Helper()
	gid := "g:" + randomFileName("")[:12]
	if _, err := db.Exec(`INSERT INTO groups(id, name, created_by) VALUES($1,$2,$3)`, gid, name, owner); err != nil {
		t.Fatal("create group:", err)
	}
	db.Exec(`INSERT INTO group_members(group_id, username, role) VALUES($1,$2,'admin')`, gid, owner)
	if member != "" {
		db.Exec(`INSERT INTO group_members(group_id, username, role) VALUES($1,$2,'member')`, gid, member)
	}
	invalidateGroup(gid)
	return gid
}

func groupNameOf(t *testing.T, gid string) string {
	t.Helper()
	var n string
	db.QueryRow(`SELECT name FROM groups WHERE id=$1`, gid).Scan(&n)
	return n
}

func TestIntegrationGroupRename(t *testing.T) {
	setupIntegration(t)
	owner, member := uniqueName("gr_own"), uniqueName("gr_mem")
	seedUsers(t, owner, member)
	gid := makeGroup(t, owner, member, uniqueName("Project "))
	ownerTok, _ := generateToken(owner)
	memberTok, _ := generateToken(member)

	// An ordinary member cannot rename the group.
	newName := uniqueName("Renamed ")
	if rr := groupPost(t, groupUpdateHandler, "/groups/update", memberTok,
		map[string]any{"group_id": gid, "name": newName}); rr.Code != http.StatusForbidden {
		t.Fatalf("member renamed the group: want 403, got %d", rr.Code)
	}

	// An admin can, and the response carries the updated card.
	rr := groupPost(t, groupUpdateHandler, "/groups/update", ownerTok,
		map[string]any{"group_id": gid, "name": newName})
	if rr.Code != http.StatusOK {
		t.Fatalf("rename: status %d: %s", rr.Code, rr.Body.String())
	}
	var got groupInfo
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Name != newName {
		t.Fatalf("response name %q, want %q", got.Name, newName)
	}
	if groupNameOf(t, gid) != newName {
		t.Fatal("the name was not persisted")
	}
	// The membership cache holds the name; without a reset members would
	// keep seeing the old one.
	if groupName(gid) != newName {
		t.Fatalf("cache still returns the old name %q", groupName(gid))
	}

	// Empty and over-long names are rejected, as they are at creation.
	for _, bad := range []string{"", "   ", strings.Repeat("x", 65)} {
		if rr := groupPost(t, groupUpdateHandler, "/groups/update", ownerTok,
			map[string]any{"group_id": gid, "name": bad}); rr.Code != http.StatusBadRequest {
			t.Fatalf("name %q: want 400, got %d", bad, rr.Code)
		}
	}
	// The name must survive the rejected attempts unchanged.
	if groupNameOf(t, gid) != newName {
		t.Fatal("a rejected attempt changed the name anyway")
	}
}

// Another group's name is taken; one's own is not.
func TestIntegrationGroupRenameConflicts(t *testing.T) {
	setupIntegration(t)
	owner := uniqueName("gc_own")
	seedUsers(t, owner)
	takenName := uniqueName("Taken ")
	makeGroup(t, owner, "", takenName)
	gid := makeGroup(t, owner, "", uniqueName("Mine "))
	tok, _ := generateToken(owner)

	if rr := groupPost(t, groupUpdateHandler, "/groups/update", tok,
		map[string]any{"group_id": gid, "name": takenName}); rr.Code != http.StatusConflict {
		t.Fatalf("taken name: want 409, got %d", rr.Code)
	}
	// Renaming to one's own name must not conflict with itself, or editing
	// only the description through the same form fails with 409.
	own := groupNameOf(t, gid)
	if rr := groupPost(t, groupUpdateHandler, "/groups/update", tok,
		map[string]any{"group_id": gid, "name": own}); rr.Code != http.StatusOK {
		t.Fatalf("rename to the same name: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestIntegrationGroupDescription(t *testing.T) {
	setupIntegration(t)
	owner := uniqueName("gd_own")
	seedUsers(t, owner)
	gid := makeGroup(t, owner, "", uniqueName("Described "))
	tok, _ := generateToken(owner)

	rr := groupPost(t, groupUpdateHandler, "/groups/update", tok,
		map[string]any{"group_id": gid, "description": "  On-call rotation, escalations  "})
	if rr.Code != http.StatusOK {
		t.Fatalf("description: status %d: %s", rr.Code, rr.Body.String())
	}
	var got groupInfo
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Description != "On-call rotation, escalations" {
		t.Fatalf("description was not trimmed: %q", got.Description)
	}
	// Over-long descriptions are rejected.
	if rr := groupPost(t, groupUpdateHandler, "/groups/update", tok,
		map[string]any{"group_id": gid, "description": strings.Repeat("x", maxGroupDescriptionLen+1)}); rr.Code != http.StatusBadRequest {
		t.Fatalf("long description: want 400, got %d", rr.Code)
	}
	// A request with no fields is an error, not a silent no-op.
	if rr := groupPost(t, groupUpdateHandler, "/groups/update", tok,
		map[string]any{"group_id": gid}); rr.Code != http.StatusBadRequest {
		t.Fatalf("request with no fields: want 400, got %d", rr.Code)
	}
}

func TestIntegrationGroupDelete(t *testing.T) {
	setupIntegration(t)
	owner, member := uniqueName("gx_own"), uniqueName("gx_mem")
	seedUsers(t, owner, member)
	gid := makeGroup(t, owner, member, uniqueName("Disband "))
	ownerTok, _ := generateToken(owner)
	memberTok, _ := generateToken(member)

	// A message in the group, to check it survives the disbanding.
	msg := Message{Type: "message", ID: uniqueName("gmsg"), From: owner, To: gid, Body: "before disbanding"}
	if _, _, err := saveMessage(msg); err != nil {
		t.Fatal("saveMessage:", err)
	}

	// An ordinary member cannot disband the group.
	if rr := groupPost(t, groupDeleteHandler, "/groups/delete", memberTok,
		map[string]any{"group_id": gid}); rr.Code != http.StatusForbidden {
		t.Fatalf("member disbanded the group: want 403, got %d", rr.Code)
	}
	if _, ok := loadGroupInfo(gid); !ok {
		t.Fatal("the group vanished after a rejected attempt")
	}

	// An admin can.
	if rr := groupPost(t, groupDeleteHandler, "/groups/delete", ownerTok,
		map[string]any{"group_id": gid}); rr.Code != http.StatusNoContent {
		t.Fatalf("disband: want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, ok := loadGroupInfo(gid); ok {
		t.Fatal("the group is still in the database")
	}
	var members int
	db.QueryRow(`SELECT COUNT(*) FROM group_members WHERE group_id=$1`, gid).Scan(&members)
	if members != 0 {
		t.Fatalf("%d members remain after disbanding", members)
	}
	// The cache must be cleared, or a disbanded group lives on in memory.
	if loadGroup(gid) != nil {
		t.Fatal("the disbanded group is still cached")
	}
	// Messages are kept deliberately: they may be needed for an
	// investigation after the group is gone.
	var msgs int
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient=$1`, gid).Scan(&msgs)
	if msgs == 0 {
		t.Fatal("the group's messages were deleted with it")
	}
}

// An admin of one group has no authority over another.
func TestIntegrationGroupAdminScope(t *testing.T) {
	setupIntegration(t)
	a, b := uniqueName("gs_a"), uniqueName("gs_b")
	seedUsers(t, a, b)
	mine := makeGroup(t, a, "", uniqueName("Own "))
	foreign := makeGroup(t, b, "", uniqueName("Other "))
	tokA, _ := generateToken(a)

	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		path string
		body map[string]any
	}{
		{"rename another group", groupUpdateHandler, "/groups/update",
			map[string]any{"group_id": foreign, "name": uniqueName("Seized ")}},
		{"disband another group", groupDeleteHandler, "/groups/delete",
			map[string]any{"group_id": foreign}},
	} {
		if rr := groupPost(t, tc.h, tc.path, tokA, tc.body); rr.Code != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d", tc.name, rr.Code)
		}
	}
	if _, ok := loadGroupInfo(foreign); !ok {
		t.Fatal("the other group was affected")
	}
	// One's own group is still manageable.
	if rr := groupPost(t, groupUpdateHandler, "/groups/update", tokA,
		map[string]any{"group_id": mine, "name": uniqueName("Own new ")}); rr.Code != http.StatusOK {
		t.Fatalf("own group: status %d", rr.Code)
	}
}

func seedUsers(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES($1,'x')
		                      ON CONFLICT (username) DO NOTHING`, n); err != nil {
			t.Fatal("seed user:", err)
		}
	}
}
