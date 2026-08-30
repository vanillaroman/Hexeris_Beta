package main

// The link preview cache: memory plus database, remembering failures too.
//
// ═══ WHAT WAS WRONG ═══════════════════════════════════════════════════════
//
// There was a cache in unfurl.go, but the quota was spent BEFORE it was used:
//
//     unfurlLimiter.recordFailure(username)   // <- the charge
//     … cache hit? …                          // <- the cache check
//
// So forty links in an open conversation ate the whole quota (40 per person
// per 10 minutes) even when every one of them was already cached and went
// nowhere. That is what produced a 429 out of nowhere: the person was not
// hammering anything, they simply scrolled a conversation full of links.
//
// Three further troubles from the same place:
//
//   1. Failures were not remembered at all. A dead link in a conversation
//      meant a fresh trip outside on every render — and failures are the
//      most expensive: five seconds of timeout each.
//
//   2. The cache lived only in process memory. After every deploy all the
//      links were fetched again — which is exactly the moment the quota is
//      short.
//
//   3. On overflow the map was cleared ENTIRELY. The five hundred and first
//      link threw away five hundred warmed entries.
//
// ═══ WHAT IT IS NOW ═══════════════════════════════════════════════════════
//
// Two layers: a map in memory (the fast path) and the unfurl_cache table (it
// survives a restart and is shared by every worker process). Failures are
// remembered separately and briefly — a site may have been down five minutes
// rather than dead forever.
//
// Plus coalescing of identical requests: ten people
// who sent one link into a group produce ONE trip outside, not ten. Without
// that a group chat turns us into a source of load on someone else's site.

import (
	"database/sql"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// A failure lives noticeably less than a success: a site may have been down
	// for five minutes, and remembering that for six hours would break previews
	// for a long time over an accident.
	unfurlFailTTL = 15 * time.Minute
	// The table ceiling. Not for space (the rows are tiny) but so that the growth
	// has a boundary at all.
	unfurlDBCap = 20000
)

func initUnfurlCacheSchema() {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS unfurl_cache (
		url_key     TEXT PRIMARY KEY,
		ok          BOOLEAN NOT NULL,
		title       TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		image       TEXT NOT NULL DEFAULT '',
		site        TEXT NOT NULL DEFAULT '',
		fetched_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		log.Println("unfurl cache schema:", err)
		return
	}
	// An index on time for the cleanup: without it deleting stale rows reads the
	// whole table.
	db.Exec(`CREATE INDEX IF NOT EXISTS idx_unfurl_cache_fetched ON unfurl_cache(fetched_at)`)
}

// unfurlKey normalises an address so that two entries about one page do not
// become two entries.
//
// Only what is certainly safe is normalised: the case of the scheme and host,
// a redundant slash, an empty fragment. The parameter order is NOT touched:
// for many sites it is significant, and "clever" normalisation would merge
// different pages into one.
func unfurlKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	// A default port is the same address.
	if (u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) ||
		(u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) {
		u.Host = u.Host[:strings.LastIndex(u.Host, ":")]
	}
	if u.Path == "/" {
		u.Path = ""
	}
	return u.String()
}

// ─── The memory layer ────────────────────────────────────────────────────

type unfurlEntry struct {
	res unfurlResult
	ok  bool // whether the fetch succeeded; false is a remembered failure
	at  time.Time
}

func (e unfurlEntry) fresh() bool {
	ttl := unfurlTTL
	if !e.ok {
		ttl = unfurlFailTTL
	}
	return time.Since(e.at) < ttl
}

// unfurlLookup looks for an entry in memory first, then in the database.
func unfurlLookup(key string) (unfurlEntry, bool) {
	unfurlMu.Lock()
	e, hit := unfurlMem[key]
	unfurlMu.Unlock()
	if hit && e.fresh() {
		return e, true
	}

	var (
		row unfurlEntry
		at  time.Time
	)
	err := db.QueryRow(
		`SELECT ok, title, description, image, site, fetched_at FROM unfurl_cache WHERE url_key=$1`,
		key).Scan(&row.ok, &row.res.Title, &row.res.Description, &row.res.Image, &row.res.Site, &at)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Println("unfurl cache read:", err)
		}
		return unfurlEntry{}, false
	}
	row.at = at
	row.res.URL = key
	if !row.fresh() {
		return unfurlEntry{}, false
	}
	unfurlMemPut(key, row)
	return row, true
}

func unfurlMemPut(key string, e unfurlEntry) {
	unfurlMu.Lock()
	defer unfurlMu.Unlock()
	if len(unfurlMem) >= unfurlCacheCap {
		unfurlEvictOldestLocked()
	}
	unfurlMem[key] = e
}

// unfurlEvictOldestLocked throws away the oldest half of the entries.
//
// Half rather than everything: the old code cleared the whole map on overflow,
// and one extra link cost five hundred warmed entries.
func unfurlEvictOldestLocked() {
	type kv struct {
		k  string
		at time.Time
	}
	all := make([]kv, 0, len(unfurlMem))
	for k, v := range unfurlMem {
		all = append(all, kv{k, v.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all)/2; i++ {
		delete(unfurlMem, all[i].k)
	}
}

// unfurlStore puts the result into both layers.
func unfurlStore(key string, res unfurlResult, ok bool) {
	e := unfurlEntry{res: res, ok: ok, at: time.Now()}
	unfurlMemPut(key, e)
	if _, err := db.Exec(
		`INSERT INTO unfurl_cache(url_key, ok, title, description, image, site, fetched_at)
		 VALUES($1,$2,$3,$4,$5,$6,NOW())
		 ON CONFLICT (url_key) DO UPDATE SET
		   ok=EXCLUDED.ok, title=EXCLUDED.title, description=EXCLUDED.description,
		   image=EXCLUDED.image, site=EXCLUDED.site, fetched_at=NOW()`,
		key, ok, res.Title, res.Description, res.Image, res.Site); err != nil {
		// The cache is an accelerator, not a source of truth: if it did not
		// write, we go outside next time. Breaking previews over that is not
		// acceptable.
		log.Println("unfurl cache write:", err)
	}
}

// ─── Coalescing identical requests ───────────────────────────────────────
//
// Ten people who sent one link into a group must produce ONE trip outside.
// Otherwise a group chat turns us into a source of load on someone else's
// site, and us into whoever that site blocks.

var (
	unfurlFlightMu sync.Mutex
	unfurlFlight   = map[string]chan struct{}{}
)

// unfurlBeginFetch says whether it is our turn to go outside. If not, it
// returns a channel that closes when someone else has been.
func unfurlBeginFetch(key string) (mine bool, wait <-chan struct{}) {
	unfurlFlightMu.Lock()
	defer unfurlFlightMu.Unlock()
	if ch, busy := unfurlFlight[key]; busy {
		return false, ch
	}
	ch := make(chan struct{})
	unfurlFlight[key] = ch
	return true, nil
}

func unfurlEndFetch(key string) {
	unfurlFlightMu.Lock()
	if ch, ok := unfurlFlight[key]; ok {
		delete(unfurlFlight, key)
		close(ch)
	}
	unfurlFlightMu.Unlock()
}

// ─── Cleanup ─────────────────────────────────────────────────────────────

// cleanUnfurlCache removes what is stale and what is surplus. Called by the
// retention janitor (retention.go) — no schedule of its own is needed.
func cleanUnfurlCache() int {
	// The stale: successes by their TTL, failures by theirs.
	res, err := db.Exec(`DELETE FROM unfurl_cache WHERE
		(ok AND fetched_at < NOW() - MAKE_INTERVAL(secs => $1)) OR
		(NOT ok AND fetched_at < NOW() - MAKE_INTERVAL(secs => $2))`,
		int(unfurlTTL.Seconds()), int(unfurlFailTTL.Seconds()))
	if err != nil {
		log.Println("unfurl cache cleanup:", err)
		return 0
	}
	n, _ := res.RowsAffected()

	// And the ceiling, in case too much accumulated within one TTL. The oldest
	// are dropped rather than random ones: fresh links matter more.
	if _, err := db.Exec(`DELETE FROM unfurl_cache WHERE url_key IN (
		SELECT url_key FROM unfurl_cache ORDER BY fetched_at DESC OFFSET $1)`, unfurlDBCap); err != nil {
		log.Println("unfurl cache cap:", err)
	}
	return int(n)
}
