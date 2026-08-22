package main

// Concurrency tests for the WS hub and the resilience of the database
// writers. No Postgres required — only the shared in-memory structures are
// exercised. Run with -race.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClient is a connection without a real socket: send() touches only
// out/quit, so Conn is unnecessary until the buffer overflows.
func testClient(username string) *Client {
	return &Client{
		Username: username,
		out:      make(chan []byte, clientSendBuffer),
		quit:     make(chan struct{}),
	}
}

// Reproduces the production scenario: a broadcast reads a user's connection
// list while a concurrent disconnect removes one of them.
//
// Editing the slice in place made the broadcast iterate the same array
// without the lock, which -race reports as "WRITE ... PREVIOUS READ" on a
// slice element. Only -race catches this; otherwise the race passes
// unnoticed almost every time.
func TestClientsSliceNoRaceOnDisconnect(t *testing.T) {
	const user = "race_user"

	mu.Lock()
	delete(clients, user)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		delete(clients, user)
		mu.Unlock()
	})

	// Several "devices" of one account.
	var pool []*Client
	mu.Lock()
	for i := 0; i < 8; i++ {
		c := testClient(user)
		pool = append(pool, c)
		clients[user] = append(clients[user], c)
	}
	mu.Unlock()

	// Drain the queues, or an overflow calls close() with a nil Conn.
	stopDrain := make(chan struct{})
	var drainWG sync.WaitGroup
	for _, c := range pool {
		drainWG.Add(1)
		go func(c *Client) {
			defer drainWG.Done()
			for {
				select {
				case <-c.out:
				case <-stopDrain:
					return
				}
			}
		}(c)
	}

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Reader: the real ACK broadcast path across all devices.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				sendACKToAll(user, "msg-1", "delivered", 1)
			}
		}
	}()

	// Writer: the same account connecting and disconnecting.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			c := testClient(user)
			mu.Lock()
			clients[user] = append(clients[user], c)
			mu.Unlock()

			drainWG.Add(1)
			go func(c *Client) {
				defer drainWG.Done()
				for {
					select {
					case <-c.out:
					case <-stopDrain:
						return
					}
				}
			}(c)

			mu.Lock()
			removeClientLocked(user, c)
			mu.Unlock()
		}
		close(done)
	}()

	wg.Wait()
	close(stopDrain)
	drainWG.Wait()

	// The original connections must be untouched.
	mu.RLock()
	got := len(clients[user])
	mu.RUnlock()
	if got != len(pool) {
		t.Fatalf("after the disconnects %d connections remain, want %d", got, len(pool))
	}
}

// A direct check that removal does not edit an already-published array: a
// snapshot taken beforehand must stay unchanged.
func TestRemoveClientLockedCopyOnWrite(t *testing.T) {
	const user = "cow_user"
	mu.Lock()
	delete(clients, user)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		delete(clients, user)
		mu.Unlock()
	})

	a, b, c := testClient(user), testClient(user), testClient(user)
	mu.Lock()
	clients[user] = []*Client{a, b, c}
	mu.Unlock()

	before := snapshotClients(user)

	mu.Lock()
	left := removeClientLocked(user, b)
	mu.Unlock()

	if left != 2 {
		t.Fatalf("%d connections remain, want 2", left)
	}
	// The snapshot taken before the removal must be intact.
	if before[0] != a || before[1] != b || before[2] != c {
		t.Fatal("the pre-removal snapshot was modified in place")
	}
	after := snapshotClients(user)
	if len(after) != 2 || after[0] != a || after[1] != c {
		t.Fatal("wrong contents after the removal")
	}

	// Removing the last connection drops the key entirely.
	mu.Lock()
	removeClientLocked(user, a)
	left = removeClientLocked(user, c)
	mu.Unlock()
	if left != 0 {
		t.Fatalf("%d connections remain, want 0", left)
	}
	mu.RLock()
	_, exists := clients[user]
	mu.RUnlock()
	if exists {
		t.Fatal("the user's key should be gone from clients")
	}
}

// One panic must not kill a database writer for good, and a waiting sender
// must not hang without an answer.
//
// saveMessage blocks on <-reply. Recovering a panic at the goroutine level
// still ends the goroutine, so the reply never arrives: the WS handler hangs
// forever and the writer pool silently shrinks until message writes stop
// altogether while /healthz keeps answering 200.
func TestMessageWriterSurvivesPanic(t *testing.T) {
	var calls atomic.Int64
	panicking := func(batch []saveRequest) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		for _, r := range batch {
			r.reply <- saveResult{seq: 42}
		}
	}

	queue := make(chan saveRequest, 4)
	go runMessageWriter(queue, panicking)

	// The first batch panics: the sender must get an error, not a hang.
	reply := make(chan saveResult, 1)
	queue <- saveRequest{msg: Message{ID: "m1"}, reply: reply}
	select {
	case res := <-reply:
		if res.err == nil {
			t.Fatal("expected a save error after the writer panicked")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sender hung: the writer died and no reply arrived")
	}

	// The writer must carry on rather than disappear.
	reply2 := make(chan saveResult, 1)
	queue <- saveRequest{msg: Message{ID: "m2"}, reply: reply2}
	select {
	case res := <-reply2:
		if res.err != nil {
			t.Fatalf("the second write should not have failed: %v", res.err)
		}
		if res.seq != 42 {
			t.Fatalf("seq=%d, want 42", res.seq)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the writer did not resume after the panic")
	}
}
