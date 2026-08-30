package main

// Batching of "delivered" marks.
//
// Each message used to cost the database two queries — the INSERT and an
// immediate UPDATE messages SET delivered=true — which doubled round trips at
// peak and drained the connection pool: a load test (80 clients, 4000
// messages in ~3s) hit pool waits a thousand times with ACK p50 near 480 ms.
//
// Batching is safe because delivered only records "handed to the socket", not
// whether the message exists. If the process dies before a flush, the message
// is simply re-delivered on the next reconnect and the client deduplicates by
// id, so nothing can be lost.

import (
	"log"
	"time"

	"github.com/lib/pq"
)

const (
	deliveredFlushInterval = 250 * time.Millisecond
	deliveredQueueCap      = 4096
	deliveredMaxBatch      = 500
)

var deliveredCh = make(chan string, deliveredQueueCap)

// markDelivered queues the mark, falling back to a synchronous write when the
// queue is full: dropping the mark would re-deliver the message on every
// reconnect from then on.
func markDelivered(id string) {
	select {
	case deliveredCh <- id:
	default:
		db.Exec("UPDATE messages SET delivered=true WHERE id=$1", id)
	}
}

func startDeliveryBatcher() {
	safeGo("deliveryBatcher", func() {
		ticker := time.NewTicker(deliveredFlushInterval)
		defer ticker.Stop()
		batch := make([]string, 0, deliveredMaxBatch)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if _, err := db.Exec("UPDATE messages SET delivered=true WHERE id = ANY($1)",
				pq.Array(batch)); err != nil {
				log.Println("delivery batch failed:", err)
			}
			batch = batch[:0]
		}
		for {
			select {
			case id := <-deliveredCh:
				batch = append(batch, id)
				if len(batch) >= deliveredMaxBatch {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	})
}
