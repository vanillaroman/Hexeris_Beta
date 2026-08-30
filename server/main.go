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

// ════════════════════════════════════════════════════════════════════════════
//  Hexeris server — cloud-chat model (à la Telegram cloud chats)
//
//  The server is the single source of truth: messages are stored centrally
//  (encrypted at rest with a server-held AES-256-GCM key, TLS in transit) and
//  every device of an account simply pulls the same plaintext-after-decrypt
//  history — no per-device sessions, no client-side key exchange required.
//  This trades "server can't read messages" for trivially reliable
//  multi-device sync, matching the brief: encryption on the server, with the
//  server as the single source of truth. A future opt-in per-chat E2E layer
//  ("secret chats") can be added later without touching this transport.
//
//  Key design decisions (vs. previous version):
//  1. messages.seq BIGSERIAL — single monotonic source of truth for ORDER.
//     Clients never sort by string id again.
//  2. messages.created_at TIMESTAMP — real timestamp shown in UI.
//  3. Full schema declared up-front + idempotent ALTERs (no silent INSERT fails).
//  4. /history?peer=X&since=N — incremental, per-conversation, ordered by seq.
//     Every device of an account calls this identically — same plaintext for
//     all of them, since the server (not the client) holds the decrypting key.
//  5. Read receipts persisted server-side (read BOOLEAN) and broadcast.
//  6. deliverPending only marks delivered after a confirmed socket write,
//     and the client additionally re-pulls via /history on every reconnect,
//     so a dropped write can never lose a message permanently.
// ════════════════════════════════════════════════════════════════════════════

// ─── Config ───────────────────────────────────────────────────────────────────

// jwtSecret is read from env JWT_SECRET. The server refuses to start
// without it — a silently-applied dev default in production would let
// anyone who has ever seen this source code forge tokens for any user.
// sync.OnceValue (not a package-level init): a log.Fatal while the package
// loads also killed `go test ./...` — the tests could not run without a
// production env. Fail-fast is preserved: main() calls mustLoadSecrets() first.
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

// dbDSN requires DATABASE_URL from the environment (a systemd drop-in at
// /etc/systemd/system/hexeris.service.d/db.conf). There used to be a fallback
// to a superuser with a default password here: when the config went missing
// the application quietly started with maximum privileges instead of failing,
// and the credentials themselves lived in git and in the binary.
func dbDSN() string {
	s := os.Getenv("DATABASE_URL")
	if s == "" {
		log.Fatal("DATABASE_URL is not set — refusing to start " +
			"(set DATABASE_URL env var)")
	}
	// A server-side ceiling on ANY query (Postgres statement_timeout): a hung or
	// locked query otherwise holds a pool connection forever, and with 50 such
	// connections the whole server stops. Respects a value already set in the DSN;
	// override with DB_STATEMENT_TIMEOUT_MS. lib/pq passes unknown parameters
	// through as session runtime parameters — this works for both the URL form
	// and key=value.
	if !strings.Contains(s, "statement_timeout") {
		ms := getEnvOrDefault("DB_STATEMENT_TIMEOUT_MS", "10000")
		if _, err := strconv.Atoi(ms); err != nil {
			log.Fatal("FATAL: DB_STATEMENT_TIMEOUT_MS must be an integer (milliseconds), got: ", ms)
		}
		s = appendDSNParam(s, "statement_timeout", ms)
	}
	// statement_timeout bounds query EXECUTION but not connection establishment.
	// When the database is down, connecting hangs on TCP retries: in a chaos
	// drill one /healthz probe in five took 5.03 s instead of 0.02 s and ran into
	// the probe timeout — monitoring saw "the check did not answer" instead of an
	// honest 503 "the database is unavailable". connect_timeout (in seconds,
	// supported by lib/pq) makes the failure fast and predictable.
	if !strings.Contains(s, "connect_timeout") {
		s = appendDSNParam(s, "connect_timeout", getEnvOrDefault("DB_CONNECT_TIMEOUT_S", "3"))
	}
	return s
}

// appendDSNParam appends a parameter to a DSN, understanding both forms:
// a URL (postgres://…?a=b) and key=value (host=… dbname=…). lib/pq passes
// parameters it does not know as session runtime parameters.
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
	// The directory is checked AT START-UP, not on the first file access. A wrong
	// path or missing permissions otherwise show up as a 404 on every image: the
	// administrator sees "file not found", goes to the directory, the file is
	// there — and the cause is never found. Here it is named at once and only
	// once.
	st, err := os.Stat(d)
	switch {
	case os.IsNotExist(err):
		log.Fatalf("FATAL: UPLOAD_DIR %q does not exist — create it and give the service user access", d)
	case err != nil:
		log.Fatalf("FATAL: UPLOAD_DIR %q is not accessible: %v", d, err)
	case !st.IsDir():
		log.Fatalf("FATAL: UPLOAD_DIR %q is not a directory", d)
	}
	// Permissions are checked by doing rather than by parsing bits: parsing lies
	// with ACLs, read-only mounts and a different running user.
	probe := filepath.Join(d, ".hexeris-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		log.Fatalf("FATAL: UPLOAD_DIR %q is not writable by the service user: %v", d, err)
	}
	os.Remove(probe)
	log.Printf("uploads: %s", d)
	return d
})

var staticDir = sync.OnceValue(func() string {
	d := os.Getenv("STATIC_DIR")
	if d == "" {
		log.Fatal("FATAL: STATIC_DIR is not set — refusing to start (set STATIC_DIR env var)")
	}
	return d
})

// mustLoadSecrets forces every mandatory env var to be read at start-up — the
// server dies at once with a clear error rather than on first use.
func mustLoadSecrets() {
	jwtSecret()
	encKey()
	adminKey()
	uploadDir()
	staticDir()
}

// ─── Types ─────────────────────────────────────────────────────────────────────

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
	// Reason is filled only in an ack with status=failed: the client used to guess
	// the reason from the peer type and lied ("you are not a member of the group")
	// even when the message had merely exceeded the length limit.
	Reason string `json:"reason,omitempty"`
	// The profile fields are filled only in type="profile" messages
	// (broadcastProfile): name, position, avatar and the manual presence status.
	DisplayName string `json:"display_name,omitempty"`
	Position    string `json:"position,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	Presence    string `json:"presence,omitempty"`
}

type Client struct {
	Username string
	Conn     *websocket.Conn
	// out is this connection's outgoing queue. A single goroutine, writeLoop,
	// reads it and writes to the socket; send() only puts a frame here and returns
	// at once. quit stops writeLoop, closeOnce makes close() idempotent.
	out       chan []byte
	quit      chan struct{}
	closeOnce sync.Once
}

// clientSendBuffer is the depth of one connection's outgoing queue. While a
// client reads faster than it is written to, the queue is nearly empty. If it
// falls so far behind that the buffer overflows, we DROP that connection rather
// than slow the broadcasts: one slow reader must not freeze the hub.
const clientSendBuffer = 256

var errClientSlow = errors.New("client send buffer full")

// newClient creates a client and starts its writer. Call it before the first
// send (deliverPending and friends), or frames sit in the buffer and never go.
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
			// localhost is local development only. TLS_MODE=http does not mean
			// "any origin": it is the normal production mode where TLS is
			// terminated by nginx in front, not a sign of a dev environment.
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
		// Calls are visible only when TURN exists AND this is not a demo. Without
		// TURN_SECRET /turn-credentials answers 503, and a call without TURN behind
		// NAT connects half the time; in a demo calls are deliberately off entirely.
		CallsEnabled: turnSecret != "",
	}
	// Rendering into a buffer: an error midway through Execute with a direct write
	// to w handed the client truncated HTML with a 200 and was lost silently.
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

// safeGo runs fn in a goroutine with a recover: a panic in a background task
// (status broadcast, push) is logged but does not kill the whole server.
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

// send NEVER blocks: it puts a frame into the outgoing queue and returns.
// There used to be a synchronous WriteMessage under c.mu with a 10 s deadline
// here, called from broadcasts under mu.RLock() (presence, typing, reactions,
// delivery). A slow or stuck reader held the write for up to 10 s, and with it
// the global mu — so the whole connect/disconnect path queued up behind it (a
// lock convoy: under load the CPU idled while throughput dropped severalfold).
// Writing is now decoupled: a per-client queue plus a single writeLoop writer.
func (c *Client) send(data []byte) error {
	// A closed connection must reject a frame BEFORE trying to write to the queue.
	// The quit check used to share a select with the write to c.out, and for a
	// closed client both branches were ready at once — Go picks such a pair with
	// equal probability. Half the time a frame went into the buffer of an already
	// DEAD writeLoop (it exits on quit): send returned nil, routeMessage counted
	// the message delivered and set delivered=true, while the client never
	// received it and would not see it in the offline queue either. A silent
	// message loss — caught by TestIntegrationPendingSurvivesFailedSocket.
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
		// The queue is full → the reader cannot keep up. Drop the connection
		// rather than slow the broadcast to everyone else. The read loop cleans up.
		statSlowClientDrops.Add(1)
		c.close()
		return errClientSlow
	}
}

// writeLoop is the ONLY writer to the socket: it serialises frames without an
// external mutex. It exits on a write error (gorilla does not recover from a
// failure) or on quit (the server closed the connection).
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

// close shuts a connection down idempotently: it closes quit (stopping
// writeLoop and waking send), then the socket itself — the read loop in
// wsHandler exits immediately and removes the client from clients.
func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.quit)
		// Conn may be absent: a client is constructed without a socket in the hub
		// tests (which only exercise the shared structures). A panic over that
		// would fail the test for a reason unrelated to the behaviour under
		// test.
		if c.Conn != nil {
			c.Conn.Close()
		}
	})
}

// ─── DB ────────────────────────────────────────────────────────────────────────

// dropRedundantIndex drops index idx on column col of the messages table, but
// ONLY if another index does the same job: single-column, on the same column,
// not partial (indpred IS NULL) and, where required, unique. Without that
// check the migration would drop the only index on an old database. It does
// nothing silently when there is no twin — that is not an error but "too soon".
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
		return // no twin (or no table yet) — leave the index as it is
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
		log.Printf("drop redundant index %s: %v", idx, err)
		return
	}
	log.Printf("dropped redundant index %s (it duplicated %s)", idx, twin)
}

// dropIndexIfCoveredBy drops the single-column index idx when a non-partial
// index coveredBy exists with THE SAME leading column: idx is then its prefix
// and costs on every write while giving reads nothing.
//
// A separate function from dropRedundantIndex: that one looks for a twin on
// exactly one column (indnatts = 1) and does not count a composite
// (recipient, seq) as a twin. The check is by the existence of both indexes,
// so on a database where the new index is missing the drop simply never happens.
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
	log.Printf("dropped index %s (covered by the prefix of index %s)", idx, coveredBy)
}

// dropIndexIfSupersededBy drops index idx when a valid index newer exists that
// replaces it. Separate from dropIndexIfCoveredBy: that one looks for a
// single-column twin by structure, whereas here the replacement is known by
// name (we created it a line above). If the new index somehow failed to build,
// the old one stays — the database is never left without an index.
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
	// The pool size depends on the machine, not the code: on a single-core
	// Postgres fifty concurrent backends merely compete for the same core.
	maxOpen := getEnvInt("DB_MAX_OPEN_CONNS", 50)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns((maxOpen + 1) / 2)
	db.SetConnMaxLifetime(30 * time.Minute)
	// Idle connections are closed LONG before their maximum age. After a Postgres
	// restart (an upgrade, a failover, an OOM kill) the pool is left with fifty
	// dead connections: database/sql discards one only when it HANDS it to a
	// query, so recovery stretched over dozens of requests — a measured drill
	// showed /healthz answering 503 for another ~2 minutes after the database was
	// already accepting connections (psql connected, the application did not).
	// ConnMaxIdleTime makes the background cleaner discard them without waiting
	// for a victim query. It also cures connections quietly killed by a NAT or a
	// firewall.
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
	// The two indexes below duplicated existing ones and were maintained on EVERY
	// insert for nothing:
	//   idx_messages_id  ≡ messages_id_key  (the constraint from `id TEXT UNIQUE`)
	//   idx_messages_seq ≡ messages_pkey    (PRIMARY KEY (seq))
	// Measured (200 clients × 100 messages): insert time 1.325 -> 0.865 ms,
	// throughput +12%.
	//
	// The drop MUST be conditional rather than unconditional: on old enough
	// databases there may be no twin. idx_messages_id was once itself the
	// migration restoring id uniqueness after idx_messages_id_recipient_device,
	// and on old databases seq was added by `ALTER TABLE ADD COLUMN seq BIGSERIAL`
	// (the line above) — that is, WITHOUT a primary key. Dropping the only index
	// there would break both id uniqueness and lookup by seq.
	dropRedundantIndex("idx_messages_id", "id", true)
	dropRedundantIndex("idx_messages_seq", "seq", false)

	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_recipient_pending ON messages(recipient) WHERE delivered = false`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_pair_seq ON messages(sender, recipient, seq)`)
	// The attachments panel. A partial index: attachments are a few per cent of
	// all messages, and a full index on media_type would be an order of magnitude
	// larger for the same benefit. The column order follows the
	// attachmentsHandler query: select by pair or group, then slice by type, then
	// walk the seq cursor downwards.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_attach_recipient
	         ON messages(recipient, media_type, seq DESC)
	         WHERE media_type <> '' AND media_type <> 'call' AND deleted = false`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_attach_sender
	         ON messages(sender, media_type, seq DESC)
	         WHERE media_type <> '' AND media_type <> 'call' AND deleted = false`)
	// peerList (the peers, on EVERY WS connect) filters on
	// `WHERE sender=$1 OR recipient=$1`. The sender=$1 branch is covered by
	// idx_messages_pair_seq, while recipient=$1 (all rows, not only pending and
	// not only groups) was covered by nothing, so Postgres did a sequential scan
	// of the whole table on every connection. As messages grew, connect time grew
	// explosively.
	//
	// Keys with seq as the second column serve BOTH equality on the recipient
	// (peerList) AND catching up on history with
	// `sender/recipient=$1 AND seq>$2 ORDER BY seq` — a range scan returns the
	// page and stops at LIMIT. Without the second column Postgres chose a scan of
	// messages_pkey with row-by-row filtering: the cost grew with the size of the
	// WHOLE table rather than of the answer (measurements and plans in
	// docs/engineering/ARCHITECTURE.md §6). INCLUDE (sender) makes the index
	// COVERING for peerList: it computes "who wrote to me" through DISTINCT
	// sender, and without sender in the index Postgres visited the heap for every
	// row for the sake of one name — 679 blocks from disk and 148 ms on a cold
	// cache, on EVERY WS connection. With INCLUDE it is an index-only scan: 15
	// blocks and 1.1 ms. An INCLUDE column lives only in the leaves, so writes
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_recipient_seq_sender
	         ON messages(recipient, seq) INCLUDE (sender)`)
	// The old (recipient, seq) without INCLUDE is fully superseded by the new one.
	dropIndexIfSupersededBy("idx_messages_recipient_seq", "idx_messages_recipient_seq_sender")
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_sender_seq ON messages(sender, seq)`)
	// The single-column recipient became a prefix of recipient_seq — keeping both
	// means paying twice on every write for the same thing.
	dropIndexIfCoveredBy("idx_messages_recipient", "idx_messages_recipient_seq_sender")
	// Group history: WHERE recipient='g:...' ORDER BY seq DESC — without this
	// index a growing database means a sequential scan of the whole table.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_group_seq ON messages(recipient, seq DESC) WHERE recipient LIKE 'g:%'`)

	// Reactions: before this table they were a pure WS relay and were lost for an
	// offline recipient forever. The PK makes the toggle idempotent.
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
	// A log of reaction events: /history is incremental by message seq, and a
	// reaction does not move seq — an offline client would miss it forever. rseq
	// gives reactions their own sync cursor; removed=true is the tombstone of a
	// removed reaction (otherwise the removal never reaches an offline recipient).
	db.Exec(`ALTER TABLE reactions ADD COLUMN IF NOT EXISTS removed BOOLEAN NOT NULL DEFAULT FALSE`)
	db.Exec(`ALTER TABLE reactions ADD COLUMN IF NOT EXISTS rseq BIGSERIAL`)
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_reactions_rseq ON reactions(rseq)`)

	initAdminSchema()
	// The encryption key is compared with the one the database is encrypted with.
	// It sits right after the schema and before requests are accepted: a mismatch
	// means silent data corruption, and not starting at all is better.
	initEncKeyGuardSchema()
	checkEncKeyUnchanged()
	// The provider binding column. Always created: an empty column costs nothing,
	// while enabling SSO then needs no migration on a running server.
	initOIDCSchema()
	// The second factor. For the same reason the schema is always created; each
	// employee enables it themselves.
	initTwoFASchema()
	// The link preview cache: it survives a restart and is shared by every worker
	// process (see unfurlcache.go).
	initUnfurlCacheSchema()
	loadLogoutCutoffs()

	// A partial index for search: it scans only textual, non-deleted messages by
	// descending seq. Without it Postgres walked the whole table, discarding media
	// and deleted rows one by one.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_search
	         ON messages(seq DESC)
	         WHERE deleted = false AND COALESCE(media_type,'') = ''`)

	// ── Groups ──
	// Group messages live in the same messages table with recipient="g:<id>": the
	// prefix does not pass usernameRe, so a collision with a user name is
	// impossible, and history, reactions, search and offline sync work without
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
	// The purpose of a group: dozens accumulate in a corporate chat, and six
	// months later the name alone no longer says how "Project 2" differs from
	// "Project 2 (new)".
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
	// Binding a Google account to a user. Before this column google-auth issued a
	// token to anyone whose email prefix matched an existing username — account
	// takeover (see googleAuthHandler).
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS google_sub TEXT`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_google_sub ON users(google_sub) WHERE google_sub IS NOT NULL`)

	// The employee profile: display name, position, avatar and the manual presence
	// status (available/busy/away — separate from online/offline, which is decided
	// by the presence of a WS connection).
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS display_name TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS position TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS phone TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS presence TEXT NOT NULL DEFAULT 'available'`)
	// The account was created by an administrator (or had its password reset) —
	// the password is known to more than its owner. While the flag is set the
	// client shows the password-change screen instead of the chat. Cleared in
	db.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS must_change_password BOOLEAN NOT NULL DEFAULT FALSE`)

	// A PARTICULAR user's preferences for one conversation: mute, archive and the
	// personal visibility boundary after a clear (see chatprefs.go). The composite
	// primary key also closes the two-tab race: both write to the same row with an
	// UPSERT rather than creating duplicates.
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
	// When the conversation was archived. Needed for the order inside the section:
	// sorting the archive by last-message time is pointless — archived chats are
	// exactly the ones nobody expects messages in, and they would be reshuffled by
	// every remark. updated_at will not do: it moves on mute as well, so the
	// conversation would jump because of a setting unrelated to archiving.
	db.Exec(`ALTER TABLE chat_prefs ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`)

	log.Println("DB connected")
}

// ─── main ─────────────────────────────────────────────────────────────────────

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
	// The shared guard: CORS, preflight, the key (header or query) and the IP filter.
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

	// Active calls: callMap holds ONE entry per call (caller -> callee); dividing
	// by 2 was a mistake — the counter always showed half.
	mu.RLock()
	activeCalls := len(callMap)
	mu.RUnlock()

	// The database pool and the runtime are the first things looked at when "the
	// server is slow": WaitCount > 0 means queries are short of pool connections.
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
		// Since the process started:
		"uptime_seconds":       int64(time.Since(serverStartTime).Seconds()),
		"messages_saved_total": statMessagesSaved.Load(),
		"ws_connects_total":    statWSConnects.Load(),
		"slow_client_drops":    statSlowClientDrops.Load(),
		"goroutines":           runtime.NumGoroutine(),
		// Batched message writes: avg_rows ≈ 1 means there is no load and batching
		// is asleep (which is normal), avg_rows ≫ 1 that it kicked in and is saving
		// the database. retries > 0 — batches that fell apart into per-row retries.
		// fast_fails > 0 — batches rejected wholesale because of an infrastructure
		// database error (not because of the data); save_timeouts > 0 — senders whose
		// messages were not saved in time and went to a retry; panics are always a
		// bug. These three counters show a write failure that is otherwise invisible:
		// /healthz stays 200 while the database is alive.
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
		// Retention settings — an operator must see them without logging into the
		// server: "how long do we keep it" is asked both by the customer's security
		// team and by the customer themselves at the first incident.
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
	// The `./server backup` subcommand — a one-off database backup, then exit. It
	// does not start the server or touch the pool: pg_dump connects by DATABASE_URL.
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		path, err := runBackupOnce()
		if err != nil {
			log.Fatal("backup failed: ", err)
		}
		log.Println("backup written:", path)
		return
	}

	// The `./hexeris verify-restore <scratch_db_url> [upload_dir]` subcommand — a
	// read-only check of a restored set (see scripts/restore-drill.sh). It does
	// not start the server and uses only encKey (package initialisation).
	if len(os.Args) > 1 && os.Args[1] == "verify-restore" {
		runVerifyRestore(os.Args[2:])
		return
	}

	// The `./hexeris check-media [-v]` subcommand — reconciling attachments with
	// the disk. Read-only. It answers "why do some pictures not open", which
	// cannot be investigated one picture at a time in a browser.
	if len(os.Args) > 1 && os.Args[1] == "check-media" {
		// NOT mustLoadSecrets: the reconciliation needs neither JWT_SECRET nor
		// ADMIN_KEY nor the static directory. Demanding them would force the
		// administrator to assemble the whole environment for a read-only check,
		// and on a production server some of those values live in a systemd
		// drop-in and are not to hand. What is needed is taken below, one by one.
		runCheckMedia(os.Args[2:])
		return
	}

	// The `./hexeris rotate-enc-key [--dry-run] [--yes]` subcommand — changing
	// SERVER_ENC_KEY while keeping the data. Also without mustLoadSecrets:
	// it needs neither JWT_SECRET nor the static directory, and on a production
	// server they live in a systemd drop-in and are not to hand.
	if len(os.Args) > 1 && os.Args[1] == "rotate-enc-key" {
		runRotateEncKey(os.Args[2:])
		return
	}

	mustLoadSecrets()
	cfg = loadConfig()
	initDB()
	initPush()

	startDeliveryBatcher()
	startMessageWriters()
	startLimiterJanitor()
	startBackupJanitor()    // a no-op while DB_BACKUP_ENABLED != true
	startRetentionJanitor() // a no-op while RETENTION_ENABLED != true
	startLoginAuditJanitor()
	loadMutedCache() // muted conversations into memory, before the first message
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
	http.HandleFunc("/attachments", attachmentsHandler)
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
	http.HandleFunc("/admin/audit-export", adminAuditExportHandler)
	// The only endpoint that hands decrypted message bodies to the outside.
	// The restrictions (mandatory user and reason, refusal when the audit write
	// fails) live in msgexport.go and are explained there.
	http.HandleFunc("/admin/message-export", adminMessageExportHandler)
	// The second factor. /auth/2fa/verify is public (the second sign-in step),
	// the rest require a valid token already.
	http.HandleFunc("/auth/2fa/status", twoFAStatusHandler)
	http.HandleFunc("/auth/2fa/setup", twoFASetupHandler)
	http.HandleFunc("/auth/2fa/enable", twoFAEnableHandler)
	http.HandleFunc("/auth/2fa/disable", twoFADisableHandler)
	http.HandleFunc("/auth/2fa/verify", twoFAVerifyHandler)
	http.HandleFunc("/admin/groups", adminGroupsHandler)
	http.HandleFunc("/admin/group-members", adminGroupMembersHandler)
	http.HandleFunc("/admin/group-action", adminGroupActionHandler)
	// An unknown admin endpoint. Without this line it fell through to the "/"
	// catch-all and got the MESSENGER's index.html with a 200 — the panel saw
	// "success" and then failed while parsing JSON. A 404 from the server for a
	// missing endpoint never arose at all.
	//
	// Hence the diagnostic conclusion that cost a day of investigation: a 404 the
	// panel DOES show could not physically have come from the messenger. It comes
	// from nginx on admin.example.com when the request missed the /admin-api/
	// location and fell into `location / { return 404; }`. The two cases could
	// not be told apart by the status code alone, and the panel confidently
	// pointed at the wrong machine.
	//
	// The marker in the body exists precisely to tell them apart: "no such
	// endpoint" (an old server) and "no route" (nginx) are fixed in different
	// places and on different hosts. The answer is served behind adminGuard — an
	// unknown source must not get an enumerator of existing endpoints.
	http.HandleFunc("/admin/", adminUnknownHandler)
	// The same set of endpoints under the prefix the panel uses. This way the
	// admin-side proxy does not rewrite the path at all — see adminAPIAliasHandler.
	http.HandleFunc(adminAPIAliasPrefix, adminAPIAliasHandler)
	// /status returns a user's existence and online status. It used to work for
	// any name — enumeration could reveal the whole staff list and who is at their
	// computer right now. The status is now visible only to those who already have
	// contact (a conversation or a shared group); for other existing users
	// "offline" is returned without revealing activity, while the bare fact of
	// existence stays available — the client needs it so it does not create a
	// conversation with a typo in the name.
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
	// Sign-in through a corporate provider (OIDC). The routes are always
	// registered: with the switch off they answer 501 with an explanation rather
	// than 404 — an administrator must see the difference between "not configured"
	// and "there is no such feature".
	http.HandleFunc("/auth/oidc/status", oidcStatusHandler)
	http.HandleFunc("/auth/oidc/start", oidcStartHandler)
	http.HandleFunc("/auth/oidc/callback", oidcCallbackHandler)
	http.HandleFunc("/auth/oidc/exchange", oidcExchangeHandler)
	http.HandleFunc("/change-password", changePasswordHandler)
	// The public config for the client. index.html may be served STATICALLY (by
	// nginx directly), in which case the Go template {{.AppName}} is not processed
	// — so the client takes the app name and the flags from here and applies them.
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
	// Employee profiles (name, position, avatar, presence status).
	http.HandleFunc("/api/profile", profileHandler)
	http.HandleFunc("/api/profiles", profilesHandler)
	http.HandleFunc("/api/presence", presenceHandler)
	// The frontend static files are laid out by type in STATIC_DIR:
	//   js/ — client modules, css/ — styles, assets/ — icons and splashes (media).
	// All three are served with no-cache so an update arrives without a manual
	// cache purge; a long-lived cache is set only on the unchanging LOGO below.
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
	// The fonts are ours, not from a CDN (why: see the comment in web/index.html).
	// The content never changes, so a long cache is used here rather than
	// no-cache: otherwise every page load would mean four dozen conditional
	// requests for files that are certainly the same.
	fontFS := http.FileServer(http.Dir(filepath.Join(staticDir(), "fonts")))
	http.Handle("/fonts/", http.StripPrefix("/fonts/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		fontFS.ServeHTTP(w, r)
	})))
	http.HandleFunc("/LOGO_DARK.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, filepath.Join(staticDir(), "LOGO_DARK.svg"))
	})
	// PWA: manifest + service worker must be served from the root with the right
	// MIME and scope (additive routes; nothing existing changes).

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

	// LISTEN_ADDR and TLS_MODE allow running in Docker without privileges:
	// TLS_MODE=tls  (the default) — HTTPS on LISTEN_ADDR (default :443)
	// TLS_MODE=http — HTTP on LISTEN_ADDR (default :8080), TLS terminated upstream
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

	// The CSP is assembled once. External origins strictly follow what the client
	// actually uses: Google GSI (script/frame/connect), link previews — any https
	// images (img-src https:), media and voice messages — blob:. Plus object and
	// embed are closed off, along with form-action to a foreign origin and being
	// framed by one (frame-ancestors).
	//
	// Google Fonts is gone from style-src/font-src: the fonts are now ours, in
	// /fonts (web/css/fonts.css). The permission would have been nothing but a
	// hole in the origin list for something the client does not use.
	//
	// script-src WITHOUT 'unsafe-inline': all client code lives in files, and
	// there are no inline handlers (onclick=) or inline <script> left in the
	// markup (see web/js/events.js — delegation by data-act). A <script> or
	// onerror= injected through XSS is now simply refused by the browser.
	// style-src 'unsafe-inline' stays for now: there are many style="" attributes
	// in the markup, clearing them is a separate task and it does not affect
	// protection against code execution.
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
		// HSTS only in TLS mode — in HTTP mode the browser would remember it and
		// force a redirect to HTTPS even in dev.
		if tlsMode == "tls" {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", csp)
		// External links from chats must not receive the full source URL; the origin
		// is kept — some OAuth and embed flows require it.
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// The camera and microphone are needed by the application itself (calls, voice
		// messages) — allowed for our own origin only; everything else is off.
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

	// Graceful shutdown: systemd sends SIGTERM on stop/restart. It used to simply
	// kill the process — every WS was torn down hard and in-flight requests
	// (upload, /history) were cut off mid-way.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")

	// 1. Close the WS politely: clients receive CloseGoingAway and go straight
	//    to a reconnect instead of waiting for the ping timeout.
	mu.RLock()
	var all []*Client
	for _, conns := range clients {
		all = append(all, conns...)
	}
	mu.RUnlock()
	closeMsg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server restart")
	for _, c := range all {
		// WriteControl is safe to call concurrently with writeLoop (a gorilla
		// guarantee), so no mutex is needed here. close() stops writeLoop.
		c.Conn.WriteControl(websocket.CloseMessage, closeMsg, time.Now().Add(2*time.Second))
		c.close()
	}

	// 2. Let ordinary HTTP requests finish, but for no longer than 10 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Println("shutdown:", err)
	}
	db.Close()
	log.Println("bye")
}
