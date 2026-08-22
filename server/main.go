package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	template "html/template"
	"log"
	mrand "math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Hexeris server — a cloud-chat model in which the server is the single
// source of truth. Messages are stored centrally (encrypted at rest with a
// server-held AES-256-GCM key, TLS in transit) and every device of an account
// pulls the same history, so multi-device sync needs no per-device sessions
// or client-side key exchange. The trade-off is deliberate: the server can
// read message bodies, which corporate archiving and audit require.
//
// Ordering and delivery rest on three invariants:
//   - messages.seq (BIGSERIAL) is the only ordering key; clients never sort
//     by string id.
//   - /history?peer=X&since=N is incremental per conversation and identical
//     for every device of an account.
//   - delivered is set only after a confirmed socket write, and clients
//     re-pull history on every reconnect, so a dropped write cannot lose a
//     message permanently.

// jwtSecret is read from env JWT_SECRET. The server refuses to start
// without it — a silently-applied dev default in production would let
// anyone who has ever seen this source code forge tokens for any user.
// Resolved lazily rather than in package init so that log.Fatal cannot kill
// `go test ./...`; main() calls mustLoadSecrets() to keep the fail-fast.
var jwtSecret = sync.OnceValue(func() string {
	if s := os.Getenv("JWT_SECRET"); s != "" {
		return s
	}
	log.Fatal("FATAL: JWT_SECRET is not set. Generate one with `openssl rand -base64 48` and export it before starting the server.")
	return ""
})

// encKey is the server's at-rest AES-256-GCM key for message bodies. The
// server is the single source of truth and the only party that ever holds
// this key — clients never see it, but it is NOT end-to-end: this protects
// data on disk/backups, not from the server operator. Refuses to start
// without a properly configured key, for the same reason as jwtSecret above.
var encKey = sync.OnceValue(func() []byte {
	s := os.Getenv("SERVER_ENC_KEY")
	if s == "" {
		log.Fatal("FATAL: SERVER_ENC_KEY is not set. Generate one with `openssl rand -base64 32` and export it before starting the server.")
	}
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(k) != 32 {
		log.Fatal("FATAL: SERVER_ENC_KEY must be base64-encoded 32 bytes (`openssl rand -base64 32`).")
	}
	return k
})

func encryptBody(plaintext string) string {
	block, err := aes.NewCipher(encKey())
	if err != nil {
		return plaintext
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return plaintext
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ct)
}

func decryptBody(stored string) string {
	if stored == "" {
		return stored
	}
	data, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return stored // pre-existing plaintext row (e.g. media URL), leave as-is
	}
	block, err := aes.NewCipher(encKey())
	if err != nil {
		return stored
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return stored
	}
	if len(data) < gcm.NonceSize() {
		return stored
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return stored
	}
	return string(pt)
}

// dbDSN requires DATABASE_URL. There is deliberately no built-in fallback:
// a default superuser DSN would let the app come up with maximum privileges
// whenever the config went missing, and would ship credentials in the binary.
func dbDSN() string {
	s := os.Getenv("DATABASE_URL")
	if s == "" {
		log.Fatal("DATABASE_URL is not set — refusing to start " +
			"(set DATABASE_URL env var)")
	}
	// A server-side ceiling on every query: without it one stuck or locked
	// query holds a pool connection forever, and fifty of them stall the
	// whole server. An explicit statement_timeout in the DSN wins.
	if !strings.Contains(s, "statement_timeout") {
		ms := getEnvOrDefault("DB_STATEMENT_TIMEOUT_MS", "10000")
		if _, err := strconv.Atoi(ms); err != nil {
			log.Fatal("FATAL: DB_STATEMENT_TIMEOUT_MS must be an integer (milliseconds), got: ", ms)
		}
		s = appendDSNParam(s, "statement_timeout", ms)
	}
	// statement_timeout bounds query execution but not connection setup.
	// With the database down, connecting hangs on TCP retries: a chaos drill
	// measured /healthz probes taking 5.03s instead of 0.02s, so monitoring
	// saw "probe timed out" instead of an honest 503. connect_timeout makes
	// that failure fast and predictable.
	if !strings.Contains(s, "connect_timeout") {
		s = appendDSNParam(s, "connect_timeout", getEnvOrDefault("DB_CONNECT_TIMEOUT_S", "3"))
	}
	return s
}

// appendDSNParam appends a parameter to a DSN in either supported form:
// URL (postgres://…?a=b) or key=value (host=… dbname=…). lib/pq forwards
// parameters it does not recognise as session runtime parameters.
func appendDSNParam(dsn, key, val string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + key + "=" + val
	}
	return dsn + " " + key + "=" + val
}

var uploadDir = sync.OnceValue(func() string {
	d := os.Getenv("UPLOAD_DIR")
	if d == "" {
		log.Fatal("FATAL: UPLOAD_DIR is not set — refusing to start (set UPLOAD_DIR env var)")
	}
	return d
})

var staticDir = sync.OnceValue(func() string {
	d := os.Getenv("STATIC_DIR")
	if d == "" {
		log.Fatal("FATAL: STATIC_DIR is not set — refusing to start (set STATIC_DIR env var)")
	}
	return d
})

// mustLoadSecrets resolves every required env var at startup, so the server
// fails with a clear message instead of at first use.
func mustLoadSecrets() {
	jwtSecret()
	encKey()
	adminKey()
	uploadDir()
	staticDir()
}

type Message struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	Body      string `json:"body,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Emoji     string `json:"emoji,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
	Forwarded bool   `json:"forwarded,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"` // unix ms
	// Reason is set only on acks with status=failed. Without it the client
	// guesses the cause from the peer type and reports the wrong one.
	Reason string `json:"reason,omitempty"`
	// Profile fields, set only on type="profile" messages (broadcastProfile).
	DisplayName string `json:"display_name,omitempty"`
	Position    string `json:"position,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	Presence    string `json:"presence,omitempty"`
}

type Client struct {
	Username string
	Conn     *websocket.Conn
	// out is this connection's outbound queue: send() only enqueues, and a
	// single writeLoop goroutine owns the socket. quit stops writeLoop;
	// closeOnce makes close() idempotent.
	out       chan []byte
	quit      chan struct{}
	closeOnce sync.Once
}

// clientSendBuffer is the depth of one connection's outbound queue. A client
// that falls far enough behind to overflow it gets dropped rather than
// throttling broadcasts: one slow reader must not freeze the hub.
const clientSendBuffer = 256

var errClientSlow = errors.New("client send buffer full")

// newClient starts the connection's writer. Call it before the first send
// (deliverPending and friends), or frames queue up with nobody draining them.
func newClient(username string, conn *websocket.Conn) *Client {
	c := &Client{
		Username: username,
		Conn:     conn,
		out:      make(chan []byte, clientSendBuffer),
		quit:     make(chan struct{}),
	}
	safeGo("clientWriteLoop", c.writeLoop)
	return c
}

var (
	cfg      *AppConfig
	clients  = make(map[string][]*Client)
	mu       sync.RWMutex
	db       *sql.DB
	upgrader = websocket.Upgrader{
		EnableCompression: getEnvBool("WS_COMPRESSION", true),
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		CheckOrigin: func(r *http.Request) bool {
			o := r.Header.Get("Origin")
			if o == "" {
				return true
			}
			// Only localhost is treated as development. TLS_MODE=http does
			// not mean "any origin": it is a normal production mode where
			// a reverse proxy terminates TLS in front of the server.
			if cfg.Domain == "localhost" {
				return true
			}
			return o == "https://"+cfg.Domain || o == "http://"+cfg.Domain
		},
	}
)

func renderTemplate(w http.ResponseWriter, path string) {
	tmpl, err := template.ParseFiles(path)
	if err != nil {
		log.Println("template parse:", path, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	data := struct {
		AppName, AppSlug, GoogleClientID string
		CallsEnabled                     bool
	}{
		AppName:        cfg.AppName,
		AppSlug:        slugify(cfg.AppName),
		GoogleClientID: cfg.GoogleClientID,
		// Calls are offered only when TURN is configured: without
		// TURN_SECRET /turn-credentials returns 503, and calls behind NAT
		// connect only intermittently.
		CallsEnabled: turnSecret != "",
	}
	// Render into a buffer first: writing straight to w turns a mid-Execute
	// error into truncated HTML served with status 200.
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		log.Println("template exec:", path, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}

func slugify(s string) string {
	out := ""
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			out += string(c)
		} else if c >= 'A' && c <= 'Z' {
			out += string(c + 32)
		} else {
			out += "-"
		}
	}
	return out
}

func generateID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), mrand.Intn(99999))
}

// safeGo runs fn in a goroutine with recover, so a panic in a background job
// (presence broadcast, push) is logged instead of taking the server down.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC recovered in %s: %v", name, r)
			}
		}()
		fn()
	}()
}

// send never blocks: it enqueues the frame and returns. Broadcasts (presence,
// typing, reactions, delivery) run under the global mu, so a synchronous
// socket write here would let one stuck reader hold that lock for its write
// deadline and stall every connect/disconnect behind it — a lock convoy that
// left the CPU idle while throughput collapsed.
func (c *Client) send(data []byte) error {
	// A closed connection must reject the frame BEFORE the enqueue attempt.
	// Checking quit in the same select as the send gives Go two ready cases
	// on a closed client and it picks either one, so half the frames land in
	// the buffer of a writeLoop that has already exited: send returns nil,
	// the message is marked delivered, and it never reaches the client nor
	// the offline queue. Covered by TestIntegrationPendingSurvivesFailedSocket.
	select {
	case <-c.quit:
		return errClientSlow
	default:
	}
	select {
	case c.out <- data:
		return nil
	case <-c.quit:
		return errClientSlow
	default:
		// Queue full: the receiver cannot keep up. Drop the connection
		// instead of slowing the broadcast; the read loop cleans up.
		statSlowClientDrops.Add(1)
		c.close()
		return errClientSlow
	}
}

// writeLoop is the only writer to the socket, which serialises frames without
// an extra mutex. It exits on a write error (gorilla connections do not
// recover from one) or on quit.
func (c *Client) writeLoop() {
	for {
		select {
		case data := <-c.out:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.TextMessage, data); err != nil {
				c.close()
				return
			}
		case <-c.quit:
			return
		}
	}
}

// close is idempotent: closing quit stops writeLoop and wakes send, and
// closing the socket makes the read loop in wsHandler exit and unregister
// the client.
func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.quit)
		// Conn may be nil: hub tests construct clients without a socket.
		if c.Conn != nil {
			c.Conn.Close()
		}
	})
}

// dropRedundantIndex drops index idx on messages(col), but only once another
// index does the same job: single-column, same column, not partial and, when
// required, unique. Without that check the migration would drop the only
// index on an old database. Doing nothing is a valid outcome, not an error.
func dropRedundantIndex(idx, col string, requireUnique bool) {
	var twin string
	err := db.QueryRow(`
		SELECT c.relname
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE i.indrelid = 'messages'::regclass
		   AND i.indnatts = 1
		   AND i.indpred IS NULL
		   AND (NOT $3 OR i.indisunique)
		   AND i.indkey[0] = (SELECT attnum FROM pg_attribute
		                       WHERE attrelid = 'messages'::regclass
		                         AND attname = $2 AND NOT attisdropped)
		   AND c.relname <> $1
		 LIMIT 1`, idx, col, requireUnique).Scan(&twin)
	if err != nil {
		return // no twin (or no table yet) — keep the index
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
		log.Printf("drop redundant index %s: %v", idx, err)
		return
	}
	log.Printf("dropped redundant index %s (duplicated %s)", idx, twin)
}

// dropIndexIfCoveredBy drops single-column index idx when a non-partial index
// coveredBy leads with the same column: idx is then its prefix and costs
// write throughput while giving reads nothing.
//
// Separate from dropRedundantIndex, which only recognises a strictly
// single-column twin and would not accept a composite (recipient, seq).
func dropIndexIfCoveredBy(idx, coveredBy string) {
	var covered bool
	err := db.QueryRow(`
		SELECT EXISTS (
		  SELECT 1
		    FROM pg_index big
		    JOIN pg_class bc  ON bc.oid = big.indexrelid
		    JOIN pg_index small ON small.indexrelid = to_regclass($1)
		   WHERE bc.relname   = $2
		     AND big.indrelid = 'messages'::regclass
		     AND big.indisvalid
		     AND big.indpred IS NULL
		     AND small.indpred IS NULL
		     AND small.indnatts = 1
		     AND big.indkey[0] = small.indkey[0]
		)`, idx, coveredBy).Scan(&covered)
	if err != nil || !covered {
		return
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
		log.Printf("drop covered index %s: %v", idx, err)
		return
	}
	log.Printf("dropped index %s (prefix-covered by %s)", idx, coveredBy)
}

// dropIndexIfSupersededBy drops idx once its named replacement exists and is
// valid. Unlike dropIndexIfCoveredBy, the replacement is known by name rather
// than inferred from structure. If the new index failed to build, the old one
// stays, so the table is never left unindexed.
func dropIndexIfSupersededBy(idx, newer string) {
	var ok bool
	if err := db.QueryRow(`
		SELECT EXISTS (
		  SELECT 1 FROM pg_index i
		    JOIN pg_class c ON c.oid = i.indexrelid
		   WHERE c.relname = $1
		     AND i.indrelid = 'messages'::regclass
		     AND i.indisvalid
		)`, newer).Scan(&ok); err != nil || !ok {
		return
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
		log.Printf("drop superseded index %s: %v", idx, err)
		return
	}
	log.Printf("dropped index %s (superseded by %s)", idx, newer)
}

func initDB() {
	var err error
	db, err = sql.Open("postgres", dbDSN())
	if err != nil {
		log.Fatal(err)
	}
	// Pool size belongs to the machine, not the code: on a single-core
	// Postgres fifty concurrent backends only contend for the same core.
	maxOpen := getEnvInt("DB_MAX_OPEN_CONNS", 50)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns((maxOpen + 1) / 2)
	db.SetConnMaxLifetime(30 * time.Minute)
	// Idle connections are retired long before their maximum lifetime.
	// After a Postgres restart the pool holds dozens of dead connections,
	// and database/sql only discards one when it hands it to a query — a
	// drill measured /healthz returning 503 for ~2 minutes after the
	// database was accepting connections again. ConnMaxIdleTime lets the
	// background cleaner drop them without a victim query, and also clears
	// connections silently killed by NAT or a firewall.
	db.SetConnMaxIdleTime(getEnvDurationSeconds("DB_CONN_MAX_IDLE_S", 60))

	if err = db.Ping(); err != nil {
		log.Fatal("db ping:", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id            SERIAL PRIMARY KEY,
			username      TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at    TIMESTAMPTZ DEFAULT NOW()
		)`)
	if err != nil {
		log.Fatal("create users:", err)
	}

	// messages: full schema declared up front. seq is the ordering key. body
	// is stored encrypted at rest (server-held AES-GCM key) — the server is
	// the single source of truth and decrypts it on the way out.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			seq        BIGSERIAL PRIMARY KEY,
			id         TEXT UNIQUE NOT NULL,
			sender     TEXT NOT NULL,
			recipient  TEXT NOT NULL,
			body       TEXT NOT NULL,
			media_type TEXT DEFAULT '',
			reply_to   TEXT DEFAULT '',
			forwarded  BOOLEAN DEFAULT FALSE,
			delivered  BOOLEAN DEFAULT FALSE,
			read       BOOLEAN DEFAULT FALSE,
			deleted    BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`)
	if err != nil {
		log.Fatal("create messages:", err)
	}
	// Idempotent migrations for older deployments.
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS seq BIGSERIAL`)
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS media_type TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS reply_to TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS forwarded BOOLEAN DEFAULT FALSE`)
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS read BOOLEAN DEFAULT FALSE`)
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS deleted BOOLEAN DEFAULT FALSE`)
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS edited BOOLEAN DEFAULT FALSE`)
	db.Exec(`ALTER TABLE messages ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ DEFAULT NOW()`)
	// Drop the now-unused per-device fan-out / E2E snapshot columns from the
	// short-lived multi-device-via-libsignal attempt — superseded by the
	// server-side encryption model above, where every device shares one row.
	db.Exec(`ALTER TABLE messages DROP COLUMN IF EXISTS sender_pub_key`)
	db.Exec(`ALTER TABLE messages DROP COLUMN IF EXISTS recipient_pub_key`)
	db.Exec(`ALTER TABLE messages DROP COLUMN IF EXISTS sender_device`)
	db.Exec(`ALTER TABLE messages DROP COLUMN IF EXISTS recipient_device`)
	db.Exec(`DROP INDEX IF EXISTS idx_messages_id_recipient_device`)
	// These two indexes duplicate constraint-backed ones and were maintained
	// on every insert for nothing:
	//   idx_messages_id  ≡ messages_id_key  (from `id TEXT UNIQUE`)
	//   idx_messages_seq ≡ messages_pkey    (PRIMARY KEY (seq))
	// Measured over 200 clients × 100 messages: insert time 1.325 → 0.865 ms,
	// throughput +12%.
	//
	// The drop is conditional because old databases may lack the twin: seq
	// was once added by plain ALTER TABLE, without a primary key. Dropping
	// the only index there would lose both id uniqueness and seq lookups.
	dropRedundantIndex("idx_messages_id", "id", true)
	dropRedundantIndex("idx_messages_seq", "seq", false)

	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_recipient_pending ON messages(recipient) WHERE delivered = false`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_pair_seq ON messages(sender, recipient, seq)`)
	// peerList runs on every WS connect and filters
	// `WHERE sender=$1 OR recipient=$1`. idx_messages_pair_seq covers the
	// sender branch; recipient=$1 was covered by nothing, so Postgres
	// sequentially scanned the whole table on each connect and connect time
	// grew with the message count.
	//
	// Keeping seq as the second column serves both that equality lookup and
	// history catch-up (`recipient=$1 AND seq>$2 ORDER BY seq`), which a
	// range scan can stop at LIMIT instead of paying for the whole table.
	// INCLUDE (sender) makes the index covering for peerList's DISTINCT
	// sender: 679 heap blocks and 148 ms on a cold cache became an
	// index-only scan of 15 blocks and 1.1 ms (docs/ARCHITECTURE.md §6).
	// An INCLUDE column lives only in leaf pages, so writes barely notice.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_recipient_seq_sender
	         ON messages(recipient, seq) INCLUDE (sender)`)
	// The older (recipient, seq) index is fully covered by the new one.
	dropIndexIfSupersededBy("idx_messages_recipient_seq", "idx_messages_recipient_seq_sender")
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_sender_seq ON messages(sender, seq)`)
	// Single-column recipient is now a prefix of recipient_seq; keeping both
	// pays for the same write twice.
	dropIndexIfCoveredBy("idx_messages_recipient", "idx_messages_recipient_seq_sender")
	// Group history (`recipient LIKE 'g:%' ORDER BY seq DESC`) would
	// otherwise scan the whole table as it grows.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_group_seq ON messages(recipient, seq DESC) WHERE recipient LIKE 'g:%'`)

	// Reactions are persisted rather than relayed over WS only, which used
	// to lose them for offline recipients. The PK makes toggling idempotent.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS reactions (
			msg_id     TEXT NOT NULL,
			username   TEXT NOT NULL,
			emoji      TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (msg_id, username, emoji)
		)`)
	if err != nil {
		log.Fatal("create reactions:", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_reactions_msg ON reactions(msg_id)`)
	// /history is incremental by message seq, and a reaction does not move
	// seq, so reactions need their own cursor: rseq. removed=true is the
	// tombstone that carries an un-reaction to an offline client.
	db.Exec(`ALTER TABLE reactions ADD COLUMN IF NOT EXISTS removed BOOLEAN NOT NULL DEFAULT FALSE`)
	db.Exec(`ALTER TABLE reactions ADD COLUMN IF NOT EXISTS rseq BIGSERIAL`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_reactions_rseq ON reactions(rseq)`)

	initAdminSchema()
	loadLogoutCutoffs()

	// Partial index for search: scanning only text, non-deleted messages by
	// descending seq, instead of filtering media and deletions row by row.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_search
	         ON messages(seq DESC)
	         WHERE deleted = false AND COALESCE(media_type,'') = ''`)

	// Group messages live in the same messages table with recipient="g:<id>".
	// The prefix cannot pass usernameRe, so it can never collide with a
	// username, and history, reactions, search and offline sync work without
	// a parallel code path.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS groups (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			created_by TEXT NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`)
	if err != nil {
		log.Fatal("create groups:", err)
	}
	// Purpose of the group: dozens accumulate in a corporate chat, and a
	// name alone stops distinguishing them within months.
	db.Exec(`ALTER TABLE groups ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT ''`)
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS group_members (
			group_id  TEXT NOT NULL,
			username  TEXT NOT NULL,
			role      TEXT NOT NULL DEFAULT 'member',
			joined_at TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (group_id, username)
		)`)
	if err != nil {
		log.Fatal("create group_members:", err)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_group_members_user ON group_members(username)`)

	// Signal Protocol key tables and users.pub_key from the abandoned E2E
	// multi-device attempt are no longer used — drop them rather than leave
	// dead schema around.
	db.Exec(`DROP TABLE IF EXISTS identity_keys`)
	db.Exec(`DROP TABLE IF EXISTS signed_prekeys`)
	db.Exec(`DROP TABLE IF EXISTS one_time_prekeys`)
	db.Exec(`ALTER TABLE users DROP COLUMN IF EXISTS pub_key`)
	// Binds a Google account to a user. Without it, google-auth issued a
	// token to anyone whose email prefix matched an existing username —
	// account takeover (see googleAuthHandler).
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS google_sub TEXT`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_google_sub ON users(google_sub) WHERE google_sub IS NOT NULL`)

	// Employee profile. presence is the manually chosen status
	// (available/busy/away), separate from online/offline, which follows the
	// existence of a WS connection.
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS position TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS phone TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS presence TEXT NOT NULL DEFAULT 'available'`)
	// The account was created (or reset) by an admin, so its password is
	// known to someone other than the owner. While the flag is set the
	// client shows the password-change screen instead of the chat; it is
	// cleared in changePasswordHandler.
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS must_change_password BOOLEAN NOT NULL DEFAULT FALSE`)

	// Per-user settings for one conversation: mute, archive and the personal
	// visibility cut-off after clearing (see chatprefs.go). The composite
	// primary key also settles the two-tabs race — both UPSERT into the same
	// row instead of creating duplicates.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chat_prefs (
			username    TEXT   NOT NULL,
			peer        TEXT   NOT NULL,
			muted       BOOLEAN NOT NULL DEFAULT FALSE,
			archived    BOOLEAN NOT NULL DEFAULT FALSE,
			cleared_seq BIGINT NOT NULL DEFAULT 0,
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (username, peer)
		)`)
	if err != nil {
		log.Fatal("create chat_prefs:", err)
	}
	// When the conversation was archived, which is what orders the archive.
	// Sorting it by last message would reshuffle it on every incoming
	// reply — exactly what archiving was meant to stop — and updated_at also
	// moves on mute, an unrelated setting.
	db.Exec(`ALTER TABLE chat_prefs ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`)

	log.Println("DB connected")
}

// adminKey protects /admin/metrics. Set via ADMIN_KEY env var.
// Generate: openssl rand -base64 32
var adminKey = sync.OnceValue(func() string {
	k := os.Getenv("ADMIN_KEY")
	if k == "" {
		log.Fatal("FATAL: ADMIN_KEY is not set. Generate: openssl rand -base64 32")
	}
	return k
})

func adminMetricsHandler(w http.ResponseWriter, r *http.Request) {
	// Shared guard: CORS, preflight, key (header or query) and IP filter.
	if !adminGuard(w, r) {
		return
	}

	// Online users — read from live WS clients map
	mu.RLock()
	onlineUsers := len(clients)
	onlineConns := 0
	for _, conns := range clients {
		onlineConns += len(conns)
	}
	mu.RUnlock()

	// DB stats
	var totalUsers, totalMessages int
	db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&totalUsers)
	db.QueryRow(`SELECT COUNT(*) FROM messages WHERE deleted = false`).Scan(&totalMessages)

	// callMap holds one entry per call (caller -> callee), so this is the
	// call count as is — halving it would under-report.
	mu.RLock()
	activeCalls := len(callMap)
	mu.RUnlock()

	// The pool is the first thing to check when the server feels slow:
	// WaitCount > 0 means queries are starved of connections.
	dbStats := db.Stats()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", cfg.AdminOrigin)
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"online_users":   onlineUsers,
		"online_conns":   onlineConns,
		"total_users":    totalUsers,
		"total_messages": totalMessages,
		"active_calls":   activeCalls,
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
		// Since process start:
		"uptime_seconds":       int64(time.Since(serverStartTime).Seconds()),
		"messages_saved_total": statMessagesSaved.Load(),
		"ws_connects_total":    statWSConnects.Load(),
		"slow_client_drops":    statSlowClientDrops.Load(),
		"goroutines":           runtime.NumGoroutine(),
		// Batched message writes. avg_rows ≈ 1 simply means there is no
		// load; avg_rows ≫ 1 means batching engaged and is sparing the
		// database. retries counts batches that fell back to row-by-row.
		// fast_fails, save_timeouts and panics are the ones that matter:
		// they expose write failures that /healthz cannot show, since it
		// stays 200 as long as the database itself is alive.
		"msg_writer": map[string]any{
			"batches":       statMsgBatches.Load(),
			"rows":          statMsgBatchRows.Load(),
			"avg_rows":      avgBatchRows(),
			"retries":       statMsgBatchRetry.Load(),
			"fast_fails":    statMsgBatchFastFail.Load(),
			"panics":        statMsgBatchPanics.Load(),
			"save_timeouts": statSaveTimeouts.Load(),
			"queued":        len(msgQueue),
			"queue_cap":     msgQueueCap,
		},
		"backup": backupState.snapshot(),
		// Retention settings, surfaced so an operator can answer "how long
		// do we keep this?" without shell access to the server.
		"retention": map[string]any{
			"enabled":            retentionEnabled(),
			"message_days":       getEnvInt("RETENTION_MESSAGE_DAYS", 365),
			"admin_audit_days":   getEnvInt("RETENTION_AUDIT_DAYS", 180),
			"login_audit_days":   getEnvInt("LOGIN_AUDIT_KEEP_DAYS", 90),
			"backup_keep":        getEnvInt("DB_BACKUP_KEEP", 7),
			"backup_every_hours": backupIntervalHours(),
		},
		"db_pool": map[string]any{
			"open":             dbStats.OpenConnections,
			"in_use":           dbStats.InUse,
			"idle":             dbStats.Idle,
			"wait_count":       dbStats.WaitCount,
			"wait_duration_ms": dbStats.WaitDuration.Milliseconds(),
			"max_open_allowed": dbStats.MaxOpenConnections,
		},
	})
}

func main() {
	// `./hexeris backup` takes one backup and exits. It never starts the
	// server or touches the pool: pg_dump connects via DATABASE_URL itself.
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		path, err := runBackupOnce()
		if err != nil {
			log.Fatal("backup failed: ", err)
		}
		log.Println("backup written:", path)
		return
	}

	// `./hexeris verify-restore <scratch_db_url> [upload_dir]` is a read-only
	// check of a restored set (see scripts/restore-drill.sh). It does not
	// start the server and needs only encKey.
	if len(os.Args) > 1 && os.Args[1] == "verify-restore" {
		runVerifyRestore(os.Args[2:])
		return
	}

	mustLoadSecrets()
	cfg = loadConfig()
	initDB()
	initPush()

	startDeliveryBatcher()
	startMessageWriters()
	startLimiterJanitor()
	startBackupJanitor()    // no-op unless DB_BACKUP_ENABLED=true
	startRetentionJanitor() // no-op unless RETENTION_ENABLED=true
	startLoginAuditJanitor()
	loadMutedCache() // muted conversations, in memory before the first message
	if ldapEnabled() {
		log.Println("LDAP/AD auth ENABLED — employees can sign in with directory credentials")
	}
	http.HandleFunc("/healthz", healthzHandler)
	http.HandleFunc("/api/session-cookie", sessionCookieHandler)
	http.HandleFunc("/api/push/vapidPublicKey", vapidPublicKeyHandler)
	http.HandleFunc("/api/push/subscribe", subscribeHandler)
	http.HandleFunc("/register", registerHandler)
	http.HandleFunc("/login", loginHandler)
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/history", historyHandler)
	http.HandleFunc("/reactions", reactionsHandler)
	http.HandleFunc("/search", searchHandler)
	http.HandleFunc("/unfurl", unfurlHandler)
	http.HandleFunc("/groups", groupsHandler)
	http.HandleFunc("/groups/members", groupMembersHandler)
	http.HandleFunc("/groups/role", groupRoleHandler)
	http.HandleFunc("/groups/update", groupUpdateHandler)
	http.HandleFunc("/groups/delete", groupDeleteHandler)
	http.HandleFunc("/groups/leave", groupLeaveHandler)
	http.HandleFunc("/chats/prefs", chatPrefsHandler)
	http.HandleFunc("/chats/clear", chatClearHandler)
	http.HandleFunc("/turn-credentials", turnCredentialsHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/files/", filesHandler)
	http.HandleFunc("/delete-message", deleteMessageHandler)
	http.HandleFunc("/edit-message", editMessageHandler)
	http.HandleFunc("/admin/metrics", adminMetricsHandler)
	http.HandleFunc("/admin/users", adminUsersHandler)
	http.HandleFunc("/admin/user-action", adminUserActionHandler)
	http.HandleFunc("/admin/login-audit", adminLoginAuditHandler)
	http.HandleFunc("/admin/audit", adminAuditHandler)
	http.HandleFunc("/admin/groups", adminGroupsHandler)
	http.HandleFunc("/admin/group-members", adminGroupMembersHandler)
	http.HandleFunc("/admin/group-action", adminGroupActionHandler)
	// /status reports whether a user exists and whether they are online.
	// Online state is limited to contacts (a shared conversation or group);
	// for everyone else the answer is a flat "offline", so the endpoint
	// cannot be enumerated to map the staff list and who is at their desk.
	// Existence stays visible because the client needs it to reject a
	// mistyped username instead of opening a dead conversation.
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		me, ok := validateToken(extractToken(r))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		user := r.URL.Query().Get("user")
		if statusLimiter.isBlocked(me) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		statusLimiter.recordFailure(me)

		if !userExists(user) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		online := false
		if user == me || hasContactWith(me, user) {
			mu.RLock()
			online = len(clients[user]) > 0
			mu.RUnlock()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"online": online})
	})
	http.HandleFunc("/google-auth", googleAuthHandler)
	http.HandleFunc("/change-password", changePasswordHandler)
	// Public client config. index.html may be served as a static file by a
	// reverse proxy, in which case the {{.AppName}} template is never
	// executed, so the client reads the app name and feature flags here.
	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		json.NewEncoder(w).Encode(map[string]any{
			"appName":             cfg.AppName,
			"callsEnabled":        turnSecret != "",
			"googleClientId":      cfg.GoogleClientID,
			"registrationEnabled": registrationEnabled(),
		})
	})
	http.HandleFunc("/api/profile", profileHandler)
	http.HandleFunc("/api/profiles", profilesHandler)
	http.HandleFunc("/api/presence", presenceHandler)
	// Front-end static files live under STATIC_DIR in js/, css/ and assets/.
	// All three are served no-cache so a deploy reaches clients without a
	// manual cache purge; only immutable assets below get a long max-age.
	serveStaticDir := func(urlPrefix, subDir string) {
		fs := http.FileServer(http.Dir(filepath.Join(staticDir(), subDir)))
		http.Handle(urlPrefix, http.StripPrefix(urlPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			fs.ServeHTTP(w, r)
		})))
	}
	serveStaticDir("/js/", "js")
	serveStaticDir("/css/", "css")
	serveStaticDir("/assets/", "assets")
	// Fonts are self-hosted rather than pulled from a CDN (see the note in
	// web/index.html). Their contents never change, so a long cache here
	// saves ~40 conditional requests per page load.
	fontFS := http.FileServer(http.Dir(filepath.Join(staticDir(), "fonts")))
	http.Handle("/fonts/", http.StripPrefix("/fonts/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		fontFS.ServeHTTP(w, r)
	})))
	http.HandleFunc("/LOGO_DARK.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, filepath.Join(staticDir(), "LOGO_DARK.svg"))
	})
	// PWA: the manifest and service worker must be served from the root with
	// the right MIME type and scope.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		renderTemplate(w, filepath.Join(staticDir(), "index.html"))
	})
	http.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "no-cache")
		renderTemplate(w, filepath.Join(staticDir(), "manifest.json"))
	})
	http.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Cache-Control", "no-cache")
		renderTemplate(w, filepath.Join(staticDir(), "sw.js"))
	})

	// LISTEN_ADDR and TLS_MODE allow running unprivileged in a container:
	//   TLS_MODE=tls  (default) — HTTPS on LISTEN_ADDR (default :443)
	//   TLS_MODE=http — HTTP on LISTEN_ADDR (default :8080), TLS terminated
	//                   by a reverse proxy
	lisAddr := os.Getenv("LISTEN_ADDR")
	tlsMode := os.Getenv("TLS_MODE")
	if tlsMode == "" {
		tlsMode = "tls"
	}
	if lisAddr == "" {
		if tlsMode == "tls" {
			lisAddr = ":443"
		} else {
			lisAddr = ":8080"
		}
	}

	// CSP is built once. External origins are listed strictly by actual use:
	// Google GSI (script/frame/connect), link previews (img-src https:),
	// media and voice messages (blob:). object/embed, cross-origin
	// form-action and framing are denied outright.
	//
	// script-src carries no 'unsafe-inline': all client code lives in files
	// and event handling is delegated by data-act (web/js/events.js), so a
	// <script> or onerror= injected via XSS simply will not execute.
	// style-src still allows inline styles — the markup has many style=""
	// attributes, and removing them does not affect code execution.
	csp := "default-src 'self'; " +
		"script-src 'self' https://accounts.google.com; " +
		"style-src 'self' 'unsafe-inline' https://accounts.google.com; " +
		"font-src 'self'; " +
		"img-src 'self' data: blob: https:; " +
		"media-src 'self' blob:; " +
		"connect-src 'self' wss://" + cfg.Domain + " ws://" + cfg.Domain + " https://accounts.google.com; " +
		"frame-src https://accounts.google.com; " +
		"object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

	secureHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HSTS only in TLS mode: sent over plain HTTP the browser would
		// remember it and force HTTPS even in local development.
		if tlsMode == "tls" {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", csp)
		// Outbound links must not carry the full source URL; the origin
		// stays because some OAuth and embed flows require it.
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// Camera and microphone are needed by the app itself (calls, voice
		// messages), so they are granted to this origin only.
		w.Header().Set("Permissions-Policy", "camera=(self), microphone=(self), geolocation=(), payment=(), usb=()")
		http.DefaultServeMux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              lisAddr,
		Handler:           secureHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if tlsMode == "tls" {
			log.Printf("server on %s (TLS)", lisAddr)
			if err := srv.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		} else {
			log.Printf("server on %s (HTTP, TLS terminated upstream)", lisAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}
	}()

	// Graceful shutdown on SIGTERM (what an init system sends on
	// stop/restart), so websockets close cleanly and in-flight requests
	// such as uploads are not cut mid-way.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")

	// Close websockets politely: clients get CloseGoingAway and reconnect
	// immediately instead of waiting for a ping timeout.
	mu.RLock()
	var all []*Client
	for _, conns := range clients {
		all = append(all, conns...)
	}
	mu.RUnlock()
	closeMsg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server restart")
	for _, c := range all {
		// gorilla guarantees WriteControl is safe concurrently with the
		// writeLoop, so no extra mutex is needed here.
		c.Conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(2*time.Second))
		c.close()
	}

	// Let plain HTTP requests finish, but not for longer than 10 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Println("shutdown:", err)
	}
	db.Close()
	log.Println("bye")
}
