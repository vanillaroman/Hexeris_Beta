package main

// The link preview cache (requires TEST_DATABASE_URL).
//
// The main point is the reason the cache was rewritten: a cache hit has no
// right to spend the quota. The old order gave a 429 to someone who had
// merely scrolled a conversation with forty links — all of them cached and
// going nowhere.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

var unfurlSchemaOnce sync.Once

func setupUnfurl(t *testing.T) {
	t.Helper()
	setupIntegration(t)
	unfurlSchemaOnce.Do(initUnfurlCacheSchema)
	unfurlMu.Lock()
	unfurlMem = map[string]unfurlEntry{}
	unfurlMu.Unlock()
}

func dropUnfurl(t *testing.T, key string) {
	t.Helper()
	t.Cleanup(func() {
		db.Exec(`DELETE FROM unfurl_cache WHERE url_key=$1`, key)
		unfurlMu.Lock()
		delete(unfurlMem, key)
		unfurlMu.Unlock()
	})
}

// ── Key normalisation ─────────────────────────────────────────────────────

func TestUnfurlKeyNormalizes(t *testing.T) {
	same := [][2]string{
		{"HTTPS://Example.COM/page", "https://example.com/page"},
		{"https://example.com/page#section", "https://example.com/page"},
		{"https://example.com:443/page", "https://example.com/page"},
		{"http://example.com:80/", "http://example.com"},
	}
	for _, p := range same {
		if unfurlKey(p[0]) != unfurlKey(p[1]) {
			t.Errorf("%q and %q produced different keys: %q vs %q",
				p[0], p[1], unfurlKey(p[0]), unfurlKey(p[1]))
		}
	}
	// These, however, are DIFFERENT pages and must not be merged: for many sites
	// the order of parameters is significant.
	diff := [][2]string{
		{"https://example.com/a", "https://example.com/b"},
		{"https://example.com/p?a=1&b=2", "https://example.com/p?b=2&a=1"},
		{"https://example.com/p", "https://other.com/p"},
	}
	for _, p := range diff {
		if unfurlKey(p[0]) == unfurlKey(p[1]) {
			t.Errorf("%q and %q were merged into one key", p[0], p[1])
		}
	}
}

// ── Storage ───────────────────────────────────────────────────────────────

func TestUnfurlStoreSurvivesMemoryLoss(t *testing.T) {
	setupUnfurl(t)
	key := "https://example.com/" + uniqueName("p")
	dropUnfurl(t, key)

	unfurlStore(key, unfurlResult{Title: "Title", Description: "Description", Site: "example.com"}, true)

	// A process restart: memory is empty, the database is not.
	unfurlMu.Lock()
	unfurlMem = map[string]unfurlEntry{}
	unfurlMu.Unlock()

	e, hit := unfurlLookup(key)
	if !hit {
		t.Fatal("the cache is empty after a restart — every link would be fetched again")
	}
	if !e.ok || e.res.Title != "Title" {
		t.Fatalf("the entry was restored incorrectly: %+v", e)
	}
	// And it is back in memory — no second trip to the database.
	unfurlMu.Lock()
	_, inMem := unfurlMem[key]
	unfurlMu.Unlock()
	if !inMem {
		t.Error("the entry from the database was not lifted into memory")
	}
}

// A failure is remembered — and not for long.
func TestUnfurlRemembersFailuresBriefly(t *testing.T) {
	setupUnfurl(t)
	key := "https://dead.example.com/" + uniqueName("p")
	dropUnfurl(t, key)

	unfurlStore(key, unfurlResult{Site: "dead.example.com"}, false)
	e, hit := unfurlLookup(key)
	if !hit {
		t.Fatal("the failure was not remembered — a dead link would cost 5 s every time")
	}
	if e.ok {
		t.Fatal("the failure was recorded as a success")
	}

	// A failure expires sooner: the site may have been down five minutes, not dead.
	if unfurlFailTTL >= unfurlTTL {
		t.Fatalf("the failure TTL %v is not shorter than the success TTL %v", unfurlFailTTL, unfurlTTL)
	}
	stale := unfurlEntry{ok: false, at: time.Now().Add(-unfurlFailTTL - time.Minute)}
	if stale.fresh() {
		t.Error("a stale failure counts as fresh")
	}
	// A success of the same age is still alive — otherwise two TTLs are pointless.
	if !(unfurlEntry{ok: true, at: time.Now().Add(-unfurlFailTTL - time.Minute)}).fresh() {
		t.Error("a success expired on the failure TTL")
	}
}

// THE MAIN POINT: a cache hit does not spend the quota.
func TestUnfurlCacheHitDoesNotSpendQuota(t *testing.T) {
	setupUnfurl(t)
	user := uniqueName("uf")
	token, _ := generateToken(user)
	key := unfurlKey("https://example.com/" + uniqueName("cached"))
	dropUnfurl(t, key)
	unfurlStore(key, unfurlResult{Title: "From the cache", Site: "example.com"}, true)

	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/unfurl?url="+key, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "198.51.100.7:1234"
		rr := httptest.NewRecorder()
		unfurlHandler(rr, req)
		return rr.Code
	}

	// Deliberately more than the quota (40 per person per 10 minutes). None of
	// these requests goes outside.
	for i := 0; i < 60; i++ {
		if code := call(); code != http.StatusOK {
			t.Fatalf("cached request #%d returned %d — the quota was eaten by cache hits", i+1, code)
		}
	}
	if unfurlLimiter.isBlocked(user) {
		t.Fatal("the limiter blocked a user who never went outside at all")
	}
}

// A remembered failure is served from the cache, with the same answer and no trip.
func TestUnfurlCachedFailureIsServedWithoutFetch(t *testing.T) {
	setupUnfurl(t)
	user := uniqueName("uf")
	token, _ := generateToken(user)
	key := unfurlKey("https://dead.example.com/" + uniqueName("x"))
	dropUnfurl(t, key)
	unfurlStore(key, unfurlResult{Site: "dead.example.com"}, false)

	req := httptest.NewRequest(http.MethodGet, "/unfurl?url="+key, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "198.51.100.8:1234"
	rr := httptest.NewRecorder()

	start := time.Now()
	unfurlHandler(rr, req)
	// A real attempt would run into the five-second timeout.
	if el := time.Since(start); el > time.Second {
		t.Errorf("the answer took %v — it looks like a trip outside happened", el)
	}
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("the remembered failure was served as %d, expected 422", rr.Code)
	}
	if unfurlLimiter.isBlocked(user) {
		t.Error("the remembered failure ate the quota")
	}
}

// ── Eviction ──────────────────────────────────────────────────────────────

// On overflow the oldest half is dropped rather than everything: the old code
// cleared the whole map, and one extra link cost five hundred warmed entries.
func TestUnfurlEvictionKeepsFreshEntries(t *testing.T) {
	unfurlMu.Lock()
	unfurlMem = map[string]unfurlEntry{}
	now := time.Now()
	for i := 0; i < unfurlCacheCap; i++ {
		unfurlMem[uniqueName("old")] = unfurlEntry{ok: true, at: now.Add(-time.Duration(i+1000) * time.Second)}
	}
	fresh := uniqueName("fresh")
	unfurlMem[fresh] = unfurlEntry{ok: true, at: now}
	unfurlEvictOldestLocked()
	left := len(unfurlMem)
	_, keptFresh := unfurlMem[fresh]
	unfurlMu.Unlock()

	if left == 0 {
		t.Fatal("eviction cleared the whole map")
	}
	if left > unfurlCacheCap {
		t.Fatalf("%d entries remain after eviction with a cap of %d", left, unfurlCacheCap)
	}
	if !keptFresh {
		t.Error("the freshest entry was evicted")
	}
}

// ── Coalescing identical requests ─────────────────────────────────────────

// Ten people with one link in a group must produce one trip outside.
func TestUnfurlSingleFlight(t *testing.T) {
	key := "https://example.com/" + uniqueName("sf")

	mine, wait := unfurlBeginFetch(key)
	if !mine || wait != nil {
		t.Fatal("the first request must go by itself")
	}
	// All the others wait.
	for i := 0; i < 5; i++ {
		m, w := unfurlBeginFetch(key)
		if m {
			t.Fatalf("request #%d also went outside — six times as much would hit the site", i+2)
		}
		if w == nil {
			t.Fatal("the waiter was given no channel")
		}
	}

	// The channel closes when the first one finishes.
	_, w := unfurlBeginFetch(key)
	unfurlEndFetch(key)
	select {
	case <-w:
	case <-time.After(time.Second):
		t.Fatal("the waiters were not woken — they hang until their own timeout")
	}

	// After completion a new trip is allowed again.
	if m, _ := unfurlBeginFetch(key); !m {
		t.Fatal("the next request could not go outside")
	}
	unfurlEndFetch(key)
}

// ── Cleanup ───────────────────────────────────────────────────────────────

func TestUnfurlCleanupRemovesStaleOnly(t *testing.T) {
	setupUnfurl(t)
	freshKey := "https://example.com/" + uniqueName("fresh")
	staleKey := "https://example.com/" + uniqueName("stale")
	dropUnfurl(t, freshKey)
	dropUnfurl(t, staleKey)

	unfurlStore(freshKey, unfurlResult{Title: "fresh"}, true)
	unfurlStore(staleKey, unfurlResult{Title: "stale"}, true)
	if _, err := db.Exec(
		`UPDATE unfurl_cache SET fetched_at = NOW() - MAKE_INTERVAL(secs => $2) WHERE url_key=$1`,
		staleKey, int(unfurlTTL.Seconds())+3600); err != nil {
		t.Fatal(err)
	}

	cleanUnfurlCache()

	count := func(k string) int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM unfurl_cache WHERE url_key=$1`, k).Scan(&n)
		return n
	}
	if count(staleKey) != 0 {
		t.Error("a stale entry survived the cleanup")
	}
	if count(freshKey) != 1 {
		t.Error("the cleanup deleted a fresh entry")
	}
}
