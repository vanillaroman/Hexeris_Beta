package main

// Deleting and editing messages.

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

func deleteMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if msgMutateLimiter.isBlocked(username) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	var req struct {
		MsgID string `json:"msg_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MsgID == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var sender, recipient string
	err := db.QueryRow("SELECT sender, recipient FROM messages WHERE id=$1", req.MsgID).Scan(&sender, &recipient)
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if sender != username {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	msgMutateLimiter.recordFailure(username) // every mutation counts
	db.Exec("UPDATE messages SET deleted=true, body='[deleted]' WHERE id=$1", req.MsgID)
	deleteMsg := Message{Type: "deleted", ID: req.MsgID, From: username, To: recipient}
	data, _ := json.Marshal(deleteMsg)
	broadcastToConversation(data, username, recipient)
	w.WriteHeader(http.StatusOK)
}

// broadcastToConversation fans an edit/delete event out to everyone in the
// conversation. The sender is included so their other devices stay in sync.
func broadcastToConversation(data []byte, sender, recipient string) {
	mu.RLock()
	defer mu.RUnlock()
	if isGroup(recipient) {
		for _, m := range groupMembers(recipient) {
			for _, c := range clients[m] {
				c.send(data)
			}
		}
	} else {
		for _, c := range clients[recipient] {
			c.send(data)
		}
		for _, c := range clients[sender] {
			c.send(data)
		}
	}
}

func editMessageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if msgMutateLimiter.isBlocked(username) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	var req struct {
		MsgID string `json:"msg_id"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MsgID == "" || strings.TrimSpace(req.Body) == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	// The same limit as WS sends (ws.go). Without it /edit-message is a
	// bypass: a short message could be "edited" to any length.
	if utf8.RuneCountInString(req.Body) > maxMessageRunes {
		http.Error(w, "message too long (max 4096 characters)", http.StatusRequestEntityTooLarge)
		return
	}
	var sender, recipient, mediaType string
	err := db.QueryRow("SELECT sender, recipient, COALESCE(media_type,'') FROM messages WHERE id=$1", req.MsgID).Scan(&sender, &recipient, &mediaType)
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if sender != username {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if mediaType != "" {
		http.Error(w, "cannot edit media message", http.StatusBadRequest)
		return
	}
	msgMutateLimiter.recordFailure(username) // every mutation counts
	// Stored encrypted at rest as in saveMessage, broadcast as plain text to
	// live clients as in routeMessage.
	if _, err := db.Exec("UPDATE messages SET body=$1, edited=true WHERE id=$2", encryptBody(req.Body), req.MsgID); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	editMsg := Message{Type: "edited", ID: req.MsgID, From: username, To: recipient, Body: req.Body}
	data, _ := json.Marshal(editMsg)
	broadcastToConversation(data, username, recipient)
	w.WriteHeader(http.StatusOK)
}
