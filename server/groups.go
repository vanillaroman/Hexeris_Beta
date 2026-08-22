package main

// Group chats: membership cache, helpers and HTTP handlers.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// maxGroupMembers bounds a group because every message fans out to all
// members, so an unbounded group is a load amplifier: one message becomes N
// deliveries and N pushes.
const maxGroupMembers = 200

func isGroup(id string) bool { return strings.HasPrefix(id, "g:") }

// Without this cache every group message costs two queries (membership and
// name) plus one more for the membership check — hundreds of extra queries a
// minute in an active group of fifty. Invalidation happens at every point
// that changes a group.

type cachedGroup struct {
	name  string
	roles map[string]string // username -> "admin" | "member"
}

var (
	groupCacheMu sync.RWMutex
	groupCache   = map[string]*cachedGroup{}
)

// loadGroup returns a group from the cache, reading through on a miss.
// nil means the group does not exist.
func loadGroup(gid string) *cachedGroup {
	groupCacheMu.RLock()
	g, ok := groupCache[gid]
	groupCacheMu.RUnlock()
	if ok {
		return g
	}

	var name string
	if db.QueryRow(`SELECT name FROM groups WHERE id=$1`, gid).Scan(&name) != nil {
		return nil // no such group
	}
	rows, err := db.Query(`SELECT username, role FROM group_members WHERE group_id=$1`, gid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	fresh := &cachedGroup{name: name, roles: map[string]string{}}
	for rows.Next() {
		var u, role string
		if rows.Scan(&u, &role) == nil {
			fresh.roles[u] = role
		}
	}

	groupCacheMu.Lock()
	groupCache[gid] = fresh
	groupCacheMu.Unlock()
	return fresh
}

// invalidateGroup drops the cached entry after any change to membership,
// roles or the group itself.
func invalidateGroup(gid string) {
	groupCacheMu.Lock()
	delete(groupCache, gid)
	groupCacheMu.Unlock()
}

func isGroupMember(gid, user string) bool {
	g := loadGroup(gid)
	if g == nil {
		return false
	}
	_, ok := g.roles[user]
	return ok
}

func isGroupAdmin(gid, user string) bool {
	g := loadGroup(gid)
	return g != nil && g.roles[user] == "admin"
}

func groupMembers(gid string) []string {
	g := loadGroup(gid)
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.roles))
	for u := range g.roles {
		out = append(out, u)
	}
	return out
}

func groupName(gid string) string {
	if g := loadGroup(gid); g != nil {
		return g.name
	}
	return ""
}

// notifyGroup pushes an event to every online member so their sidebar
// updates without a reload; offline members learn from GET /groups.
func notifyGroup(gid, ev string) {
	// Invalidating here, before reading the membership, means every code
	// path that changes a group refreshes the cache by virtue of announcing
	// the change — and the fresh membership is needed anyway, so the event
	// reaches members who were just added.
	invalidateGroup(gid)
	members := groupMembers(gid)
	payload, _ := json.Marshal(map[string]string{"type": ev, "group_id": gid})
	mu.RLock()
	defer mu.RUnlock()
	for _, m := range members {
		for _, c := range clients[m] {
			c.send(payload)
		}
	}
}

type groupInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// What the group is for: dozens accumulate in a corporate chat, and a
	// name alone stops distinguishing them within months.
	Description string            `json:"description"`
	CreatedBy   string            `json:"created_by"`
	Members     map[string]string `json:"members"` // username -> role
}

func loadGroupInfo(gid string) (groupInfo, bool) {
	g := groupInfo{ID: gid, Members: map[string]string{}}
	if db.QueryRow(`SELECT name, COALESCE(description,''), created_by FROM groups WHERE id=$1`,
		gid).Scan(&g.Name, &g.Description, &g.CreatedBy) != nil {
		return g, false
	}
	rows, err := db.Query(`SELECT username, role FROM group_members WHERE group_id=$1`, gid)
	if err != nil {
		return g, false
	}
	defer rows.Close()
	for rows.Next() {
		var u, r string
		if rows.Scan(&u, &r) == nil {
			g.Members[u] = r
		}
	}
	return g, true
}

// validGroupName is shared by creation and renaming, so a rename cannot
// introduce a name that creation would have rejected.
func validGroupName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	return name, name != "" && len([]rune(name)) <= 64
}

// groupNameTaken reports whether another group holds the name. exceptID
// keeps a rename to one's own name — a case change, say — from colliding
// with itself.
func groupNameTaken(name, exceptID string) bool {
	var exists bool
	db.QueryRow(`SELECT EXISTS(SELECT 1 FROM groups WHERE name=$1 AND id <> $2)`,
		name, exceptID).Scan(&exists)
	return exists
}

const maxGroupDescriptionLen = 280

func groupsHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := db.Query(`SELECT group_id FROM group_members WHERE username=$1`, username)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
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
		out := []groupInfo{}
		for _, id := range ids {
			if g, ok := loadGroupInfo(id); ok {
				out = append(out, g)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)

	case http.MethodPost:
		var req struct {
			Name    string   `json:"name"`
			Members []string `json:"members"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var okName bool
		if req.Name, okName = validGroupName(req.Name); !okName {
			http.Error(w, "invalid group name", http.StatusBadRequest)
			return
		}
		// The size limit must apply at creation too, not only when adding
		// members later: one request with 100k names is 100k queries.
		if len(req.Members) > maxGroupMembers-1 {
			http.Error(w, fmt.Sprintf("group is limited to %d members", maxGroupMembers), http.StatusConflict)
			return
		}
		if groupCreateLimiter.isBlocked(username) {
			http.Error(w, "too many groups created, try again later", http.StatusTooManyRequests)
			return
		}
		// Group names are unique.
		if groupNameTaken(req.Name, "") {
			http.Error(w, "group with this name already exists", http.StatusConflict)
			return
		}
		gid := "g:" + randomFileName("")[:12]
		if _, err := db.Exec(`INSERT INTO groups(id, name, created_by) VALUES($1,$2,$3)`,
			gid, req.Name, username); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		groupCreateLimiter.recordFailure(username) // every creation counts
		db.Exec(`INSERT INTO group_members(group_id, username, role) VALUES($1,$2,'admin')`, gid, username)
		for _, m := range req.Members {
			if m != username && userExists(m) {
				db.Exec(`INSERT INTO group_members(group_id, username) VALUES($1,$2) ON CONFLICT DO NOTHING`, gid, m)
			}
		}
		notifyGroup(gid, "group-changed")
		g, _ := loadGroupInfo(gid)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(g)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// POST /groups/members {"group_id","add":[],"remove":[]} — admins only.
// POST /groups/role    {"group_id","username","role"}    — admins only.
// POST /groups/leave   {"group_id"}                      — any member.
func groupMembersHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		GroupID string   `json:"group_id"`
		Add     []string `json:"add"`
		Remove  []string `json:"remove"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || !isGroup(req.GroupID) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !isGroupAdmin(req.GroupID, username) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if len(req.Add) > 0 {
		var current int
		db.QueryRow(`SELECT COUNT(*) FROM group_members WHERE group_id=$1`, req.GroupID).Scan(&current)
		if current+len(req.Add) > maxGroupMembers {
			http.Error(w, fmt.Sprintf("group is limited to %d members", maxGroupMembers),
				http.StatusConflict)
			return
		}
	}
	for _, m := range req.Add {
		if userExists(m) {
			db.Exec(`INSERT INTO group_members(group_id, username) VALUES($1,$2) ON CONFLICT DO NOTHING`,
				req.GroupID, m)
		}
	}
	removed := []string{}
	for _, m := range req.Remove {
		if m == username {
			continue // leaving oneself goes through /groups/leave
		}
		db.Exec(`DELETE FROM group_members WHERE group_id=$1 AND username=$2`, req.GroupID, m)
		removed = append(removed, m)
	}
	notifyGroup(req.GroupID, "group-changed")
	// Removed users are no longer members, so notifyGroup cannot reach
	// them. Without a direct event they would keep writing to the group
	// until their next reload and collect failed sends.
	if len(removed) > 0 {
		payload, _ := json.Marshal(map[string]string{"type": "group-changed", "group_id": req.GroupID})
		mu.RLock()
		for _, m := range removed {
			for _, c := range clients[m] {
				c.send(payload)
			}
		}
		mu.RUnlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /groups/update {"group_id","name","description"} — group admins only.
//
// Without renaming, a typo or a change of purpose can only be fixed by
// creating another group and moving people by hand, losing the history.
func groupUpdateHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		GroupID string  `json:"group_id"`
		Name    *string `json:"name"`        // nil = leave unchanged
		Descr   *string `json:"description"` // nil = leave unchanged
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || !isGroup(req.GroupID) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Permissions are checked before existence: a 404 against a 403 would
	// tell an outsider which group ids exist.
	if !isGroupAdmin(req.GroupID, username) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if req.Name == nil && req.Descr == nil {
		http.Error(w, "nothing to update", http.StatusBadRequest)
		return
	}

	if req.Name != nil {
		name, okName := validGroupName(*req.Name)
		if !okName {
			http.Error(w, "invalid group name", http.StatusBadRequest)
			return
		}
		if groupNameTaken(name, req.GroupID) {
			http.Error(w, "group with this name already exists", http.StatusConflict)
			return
		}
		if _, err := db.Exec(`UPDATE groups SET name=$1 WHERE id=$2`, name, req.GroupID); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	}
	if req.Descr != nil {
		d := strings.TrimSpace(*req.Descr)
		if len([]rune(d)) > maxGroupDescriptionLen {
			http.Error(w, fmt.Sprintf("description is limited to %d characters", maxGroupDescriptionLen),
				http.StatusBadRequest)
			return
		}
		if _, err := db.Exec(`UPDATE groups SET description=$1 WHERE id=$2`, d, req.GroupID); err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
	}

	// The name lives in the membership cache; without a reset members would
	// see the old one until the server restarts.
	invalidateGroup(req.GroupID)
	notifyGroup(req.GroupID, "group-changed")
	g, _ := loadGroupInfo(req.GroupID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(g)
}

// POST /groups/delete {"group_id"} disbands a group; admins only.
//
// The alternative — removing every member one by one until the group empties
// itself — is fifty requests and an operator mistake waiting to happen.
//
// Messages are deliberately kept: a conversation may be needed for an
// investigation after the group is gone.
func groupDeleteHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || !isGroup(req.GroupID) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !isGroupAdmin(req.GroupID, username) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	// Notify before deleting: notifyGroup reads group_members, so after the
	// DELETE there would be nobody left to tell.
	notifyGroup(req.GroupID, "group-changed")
	db.Exec(`DELETE FROM group_members WHERE group_id=$1`, req.GroupID)
	db.Exec(`DELETE FROM groups WHERE id=$1`, req.GroupID)
	invalidateGroup(req.GroupID)
	w.WriteHeader(http.StatusNoContent)
}

func groupRoleHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		GroupID  string `json:"group_id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || (req.Role != "admin" && req.Role != "member") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !isGroupAdmin(req.GroupID, username) {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if req.Username == username && req.Role == "member" {
		// Dropping one's own admin role requires another admin to remain.
		var admins int
		db.QueryRow(`SELECT COUNT(*) FROM group_members WHERE group_id=$1 AND role='admin'`,
			req.GroupID).Scan(&admins)
		if admins < 2 {
			http.Error(w, "group needs at least one admin", http.StatusConflict)
			return
		}
	}
	db.Exec(`UPDATE group_members SET role=$1 WHERE group_id=$2 AND username=$3`,
		req.Role, req.GroupID, req.Username)
	notifyGroup(req.GroupID, "group-changed")
	w.WriteHeader(http.StatusNoContent)
}

func groupLeaveHandler(w http.ResponseWriter, r *http.Request) {
	username, ok := validateToken(extractToken(r))
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		GroupID string `json:"group_id"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !isGroupMember(req.GroupID, username) {
		http.Error(w, "not a member", http.StatusForbidden)
		return
	}
	db.Exec(`DELETE FROM group_members WHERE group_id=$1 AND username=$2`, req.GroupID, username)

	// A group must not be left without an admin, and an empty group is
	// removed; its messages stay, so history survives a re-creation.
	var members, admins int
	db.QueryRow(`SELECT COUNT(*) FROM group_members WHERE group_id=$1`, req.GroupID).Scan(&members)
	if members == 0 {
		db.Exec(`DELETE FROM groups WHERE id=$1`, req.GroupID)
		// This early return skips notifyGroup, so the cache is cleared by
		// hand; otherwise the deleted group stays in memory forever.
		invalidateGroup(req.GroupID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	db.QueryRow(`SELECT COUNT(*) FROM group_members WHERE group_id=$1 AND role='admin'`, req.GroupID).Scan(&admins)
	if admins == 0 {
		db.Exec(`UPDATE group_members SET role='admin' WHERE (group_id, username) IN
			(SELECT group_id, username FROM group_members WHERE group_id=$1 ORDER BY joined_at ASC LIMIT 1)`,
			req.GroupID)
	}
	notifyGroup(req.GroupID, "group-changed")
	w.WriteHeader(http.StatusNoContent)
}

// Reactions sync incrementally on their own rseq cursor and carry both
// additions and removals.
