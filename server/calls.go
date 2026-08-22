package main

// WebRTC calls: signal queue for a callee who is briefly offline.

import (
	"sync"
	"time"
)

// call-offer/call-ice frames for a callee without a live WS used to be
// dropped silently — the main cause of "the call never arrived". They now
// wait in memory for up to 45 seconds (longer than a caller keeps ringing)
// and are flushed the moment the callee opens a socket, typically after
// tapping the incoming-call push.

const callQueueTTL = 45 * time.Second

// callQueueCap bounds one callee's queue. A real call is an offer plus a few
// dozen ICE candidates; without a cap a caller could spend 45 seconds filling
// server memory with 64 KB frames.
const callQueueCap = 40

type queuedSignal struct {
	data []byte
	at   time.Time
}

var (
	callQueueMu sync.Mutex
	callQueues  = map[string][]queuedSignal{} // callee -> pending signals
)

func queueCallSignal(to string, data []byte, isOffer bool) {
	callQueueMu.Lock()
	defer callQueueMu.Unlock()
	if isOffer {
		// A new call discards the tail of the previous unanswered one.
		callQueues[to] = []queuedSignal{{data, time.Now()}}
		return
	}
	// ICE candidates are only meaningful while an offer is queued.
	if q := callQueues[to]; len(q) > 0 && len(q) < callQueueCap {
		callQueues[to] = append(q, queuedSignal{data, time.Now()})
	}
}

func clearCallQueue(to string) {
	callQueueMu.Lock()
	delete(callQueues, to)
	callQueueMu.Unlock()
}

func flushCallSignals(username string, client *Client) {
	callQueueMu.Lock()
	q := callQueues[username]
	delete(callQueues, username)
	callQueueMu.Unlock()
	if len(q) == 0 || time.Since(q[0].at) > callQueueTTL {
		return // offer expired; the caller already saw "no answer"
	}
	for _, sig := range q {
		client.send(sig.data)
	}
}

func cleanupCallQueues() {
	callQueueMu.Lock()
	defer callQueueMu.Unlock()
	for to, q := range callQueues {
		if len(q) == 0 || time.Since(q[0].at) > callQueueTTL {
			delete(callQueues, to)
		}
	}
}

// callMap tracks active calls as caller -> callee, one entry per call, under
// the same mu as clients. Entries are added on call-offer and removed on
// call-end/call-reject or when a participant's last WS connection drops.
// An answer deliberately keeps the entry: it suppresses glare from a repeated
// offer while the call is up.
