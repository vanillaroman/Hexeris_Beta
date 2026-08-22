package main

// Batched message writes.
//
// Profiling under load (200 clients × 100 messages, one core per component)
// found Postgres saturated at 100% CPU with the application at 30%, and
// pg_stat_statements attributed 91% of all database time to the row-by-row
// INSERT into messages at 1.0–1.6 ms each.
//
// A microbenchmark on the real schema (20000 rows):
//
//	one by one       1532 rows/s    0.346 ms of database CPU per row
//	batches of 50    9069 rows/s    0.082 ms
//	batches of 200  13002 rows/s    0.058 ms  (6× cheaper)
//
// The fixed costs — statement parsing, transaction begin/commit and the WAL
// flush — are paid once per batch instead of once per message.
//
// Batching adds no latency because there is no "wait N ms for more" timer.
// A writer takes the first message blocking, then tops the batch up only
// with what is already queued. Under light load the queue is empty and a
// batch is one message; under heavy load the next batch accumulates while
// the previous insert runs, growing with the load. It engages exactly when
// needed and costs nothing when not.
//
// Multiple writers are safe even though seq is assigned in insert order: one
// connection has exactly one save in flight, because wsHandler reads the
// socket sequentially and routeMessage waits for the save result. Two
// messages from the same sender therefore cannot be reordered across writers.

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
)

const (
	// Rows per batch. Growing this further does not pay: the per-row gain
	// plateaus while the cost of a failure (retrying the batch) rises.
	// Postgres' 65535-parameter limit is far away at 7 parameters per row.
	msgBatchMaxRows = 200
	// Queue depth. Overflow means the database is not keeping up, and the
	// sender then waits on the channel so back-pressure reaches the client
	// naturally instead of an unbounded in-memory queue.
	msgQueueCap = 4096
	// Parallel writers. More writers ride out a single slow insert
	// (checkpoint, lock) better, but make batches smaller and contend more
	// on a single-core Postgres.
	msgWriters = 4

	// Waiting limits in saveMessage. Their purpose is not speed but keeping
	// a WS handler goroutine from hanging forever when the database fails.
	//
	// The queue only fills when writers are stalled, so 5 seconds is well
	// past any healthy wait and well within a user's patience.
	//
	// 30 seconds is comfortably more than statement_timeout plus queue wait,
	// so an honestly slow query still completes instead of being discarded.
	saveEnqueueTimeout = 5 * time.Second
	saveReplyTimeout   = 30 * time.Second
)

type saveResult struct {
	seq       int64
	createdAt int64
	err       error
}

type saveRequest struct {
	msg   Message
	body  string // already encrypted
	reply chan saveResult
}

var (
	msgQueue = make(chan saveRequest, msgQueueCap)

	// Average batch size is the main signal that batching is engaged:
	// batches≈rows means there is no load, rows/batches ≫ 1 means it is
	// doing its job.
	statMsgBatches    atomic.Int64
	statMsgBatchRows  atomic.Int64
	statMsgBatchRetry atomic.Int64 // batches that fell back to row-by-row

	// Batches rejected whole because of an infrastructure error: growth here
	// means the problem is Postgres or the network, not the data.
	statMsgBatchFastFail atomic.Int64
	// Panics inside a batch write. Any non-zero value is a bug.
	statMsgBatchPanics atomic.Int64
)

// avgBatchRows is the mean batch size since process start.
func avgBatchRows() float64 {
	b := statMsgBatches.Load()
	if b == 0 {
		return 0
	}
	return float64(statMsgBatchRows.Load()) / float64(b)
}

func startMessageWriters() {
	for i := 0; i < msgWriters; i++ {
		safeGo("messageWriter", func() { runMessageWriter(msgQueue, flushMessageBatch) })
	}
}

// runMessageWriter is one writer's loop. It takes the queue and flush
// function as parameters so panic behaviour can be tested without Postgres.
func runMessageWriter(queue chan saveRequest, flush func([]saveRequest)) {
	batch := make([]saveRequest, 0, msgBatchMaxRows)
	for {
		// Block on the first message: an idle writer sleeps.
		req, ok := <-queue
		if !ok {
			return
		}
		batch = append(batch[:0], req)
		// Top up with what is already queued, never waiting for more.
	drain:
		for len(batch) < msgBatchMaxRows {
			select {
			case r, ok := <-queue:
				if !ok {
					break drain
				}
				batch = append(batch, r)
			default:
				break drain
			}
		}
		safeFlush(batch, flush)
	}
}

// safeFlush guarantees two properties:
//
//  1. A panic inside a write does not kill the writer. Recovering at the
//     goroutine level still ends the goroutine, so the writer pool would
//     silently shrink and message writes would stop entirely while /healthz
//     kept answering 200, the database being alive.
//  2. Every waiter gets a reply. saveMessage blocks on <-reply, so a writer
//     dying before replying hangs a WS handler goroutine forever.
//
// The reply send is non-blocking: the channel is buffered for one value, and
// if the result was already sent before the panic the select does nothing.
func safeFlush(batch []saveRequest, flush func([]saveRequest)) {
	defer func() {
		if r := recover(); r != nil {
			statMsgBatchPanics.Add(1)
			log.Printf("PANIC recovered in messageWriter (batch of %d): %v", len(batch), r)
			for _, req := range batch {
				select {
				case req.reply <- saveResult{err: errWriterPanic}:
				default:
				}
			}
		}
	}()
	flush(batch)
}

// flushMessageBatch writes a batch as one INSERT and distributes the results.
func flushMessageBatch(batch []saveRequest) {
	// Deduplication inside the batch is mandatory: ON CONFLICT DO UPDATE
	// cannot touch the same row twice in one statement, so a single retried
	// id from a client would fail the entire batch. Duplicates are not
	// discarded — they wait for the same result as the original.
	uniq := make([]saveRequest, 0, len(batch))
	waiters := make(map[string][]chan saveResult, len(batch))
	for _, r := range batch {
		if _, seen := waiters[r.msg.ID]; !seen {
			uniq = append(uniq, r)
		}
		waiters[r.msg.ID] = append(waiters[r.msg.ID], r.reply)
	}

	rows, err := insertMessageRows(uniq)
	if err != nil && !rowLevelFailure(err) {
		// An infrastructure failure (database down, statement_timeout,
		// closed connection) is not any row's fault. Retrying row by row
		// here would mean 200 sequential attempts, each waiting out its own
		// timeout — tens of minutes during which this writer is dead and,
		// with all writers stuck, the queue fills and senders hang. Failing
		// fast instead gives the sender an ACK failure to retry with the
		// same id, which ON CONFLICT makes idempotent.
		statMsgBatchFastFail.Add(1)
		log.Printf("message batch of %d failed, infrastructure error (%v) — no row-by-row retry", len(uniq), err)
		for _, r := range uniq {
			deliverSaveResult(waiters, r.msg.ID, saveResult{err: err})
		}
		return
	}
	if err != nil {
		// A batch must not be a single point of failure: one bad row may not
		// take the other 199 with it.
		statMsgBatchRetry.Add(1)
		log.Printf("message batch of %d failed (%v) — retrying row by row", len(uniq), err)
		for _, r := range uniq {
			one, err1 := insertMessageRows([]saveRequest{r})
			res := saveResult{err: err1}
			if err1 == nil {
				if got, ok := one[r.msg.ID]; ok {
					res = got
				}
			}
			deliverSaveResult(waiters, r.msg.ID, res)
		}
		return
	}

	statMsgBatches.Add(1)
	statMsgBatchRows.Add(int64(len(uniq)))
	for _, r := range uniq {
		res, ok := rows[r.msg.ID]
		if !ok {
			// RETURNING gave no row: treat the save as failed rather than
			// ACK a message that was never stored.
			res = saveResult{err: errNoReturningRow}
		}
		deliverSaveResult(waiters, r.msg.ID, res)
	}
}

func deliverSaveResult(waiters map[string][]chan saveResult, id string, res saveResult) {
	for _, ch := range waiters[id] {
		ch <- res // buffered for one value, so this never blocks
	}
	delete(waiters, id)
}

var (
	errNoReturningRow = errors.New("insert returned no row for message id")
	errWriterPanic    = errors.New("message writer panicked while saving batch")
	errSaveOverloaded = errors.New("save queue full: database not keeping up")
	errSaveTimeout    = errors.New("save timed out waiting for database")
)

// Messages rejected on a save timeout. Non-zero means the database is not
// coping and senders are retrying; read it together with db_pool.wait_count.
var statSaveTimeouts atomic.Int64

// rowLevelFailure reports whether a specific row caused the error.
//
// Row-by-row retry only makes sense when the data is at fault: an integrity
// violation (class 23) or an invalid value (class 22). Everything else —
// network, closed pool, cancelled context, statement_timeout — has nothing to
// do with any row, and retrying individually is both pointless and harmful.
func rowLevelFailure(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false // network, driver or context, not the row
	}
	switch pqErr.Code.Class() {
	case "23", "22":
		return true
	}
	return false
}

// insertMessageRows runs one multi-row INSERT and returns seq/created_at by
// id. ON CONFLICT(id) DO UPDATE rather than DO NOTHING so RETURNING also
// yields a row for a resent id: a retrying client must get the same seq, not
// a save failure.
func insertMessageRows(reqs []saveRequest) (map[string]saveResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO messages(id, sender, recipient, body, media_type, reply_to, forwarded, delivered) VALUES `)
	args := make([]any, 0, len(reqs)*7)
	for i, r := range reqs {
		if i > 0 {
			sb.WriteByte(',')
		}
		n := i * 7
		sb.WriteString("($" + strconv.Itoa(n+1) + ",$" + strconv.Itoa(n+2) + ",$" + strconv.Itoa(n+3) +
			",$" + strconv.Itoa(n+4) + ",$" + strconv.Itoa(n+5) + ",$" + strconv.Itoa(n+6) +
			",$" + strconv.Itoa(n+7) + ",false)")
		args = append(args, r.msg.ID, r.msg.From, r.msg.To, r.body,
			r.msg.MediaType, r.msg.ReplyTo, r.msg.Forwarded)
	}
	sb.WriteString(` ON CONFLICT(id) DO UPDATE SET id=messages.id RETURNING id, seq, created_at`)

	rows, err := db.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]saveResult, len(reqs))
	for rows.Next() {
		var id string
		var seq int64
		var createdAt time.Time
		if err := rows.Scan(&id, &seq, &createdAt); err != nil {
			return nil, err
		}
		out[id] = saveResult{seq: seq, createdAt: createdAt.UnixMilli()}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
