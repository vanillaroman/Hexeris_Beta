package main

// Lightweight observability with no external dependencies: atomic counters
// since process start, plus /healthz. A Prometheus client is a deliberate
// non-choice — it would add a dependency and a second metrics format, while
// /admin/metrics already serves JSON that anything can scrape.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

var (
	serverStartTime = time.Now()

	// Counters since process start; a restart resets them. Absolute totals
	// live in the database — these are operational rates.
	statMessagesSaved atomic.Int64 // successful saveMessage calls
	statWSConnects    atomic.Int64 // WS connections accepted
	// Connections dropped because their outbound queue overflowed. Growth
	// under load means receivers are slower than the server, which is worth
	// distinguishing from an actual server ceiling.
	statSlowClientDrops atomic.Int64
)

// healthzHandler is the liveness/readiness endpoint for init systems, proxy
// upstream checks and external monitoring. It is unauthenticated because it
// exposes service state only — never user or content statistics.
//
// Two modes:
//
//	GET /healthz        → "ok" (200) | "db unreachable" (503)
//	GET /healthz?v=1    → JSON with a per-component breakdown
//
// A degraded status still returns 200. "The backup is stale" does not mean
// "the service is down", and an uptime monitor should not page anyone at
// night for it; alerts that cry wolf stop being read. Degraded is still
// detectable without parsing the body, via the X-Health-Status header.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	// Health is public and returns only state, so a status widget hosted on
	// another origin can read the response code and tell a 503 (database
	// down) apart from a network failure.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")

	// Two attempts, not one. The first may draw a stale pooled connection,
	// which is typical right after a Postgres restart: the database accepts
	// connections again while the pool still holds dead ones. database/sql
	// discards the failed connection, so the retry runs on a live one. A
	// real outage fails both and the 503 stays honest.
	var dbErr error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		dbErr = db.PingContext(ctx)
		cancel()
		if dbErr == nil {
			break
		}
	}

	checks, status := healthChecks(dbErr)
	w.Header().Set("X-Health-Status", status)

	verbose := r.URL.Query().Get("v") != "" || r.URL.Query().Get("verbose") != ""
	if !verbose {
		if status == healthDown {
			log.Printf("healthz: database unreachable: %v", dbErr)
			http.Error(w, "db unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
		return
	}

	code := http.StatusOK
	if status == healthDown {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         status,
		"checks":         checks,
		"uptime_seconds": int64(time.Since(serverStartTime).Seconds()),
	})
}

const (
	healthOK       = "ok"
	healthDegraded = "degraded"
	healthDown     = "down"
)

// healthChecks assembles per-component state and the summary status.
//
// degraded means the service responds but something is wrong in a way
// outsiders cannot see. Those failures are the dangerous ones: an outage is
// noticed immediately, whereas a backup that silently stopped running is
// discovered on the day it is needed.
func healthChecks(dbErr error) (map[string]any, string) {
	checks := map[string]any{}
	status := healthOK

	if dbErr != nil {
		checks["db"] = map[string]any{"status": healthDown, "error": dbErr.Error()}
		// With the database down nothing else matters.
		return checks, healthDown
	}
	checks["db"] = map[string]any{"status": healthOK}

	// Message writes. A panic is always a bug; save timeouts mean senders
	// are getting ack=failed and retrying.
	writer := map[string]any{"status": healthOK}
	if p := statMsgBatchPanics.Load(); p > 0 {
		writer["status"] = healthDegraded
		writer["panics"] = p
		status = healthDegraded
	}
	if t := statSaveTimeouts.Load(); t > 0 {
		writer["status"] = healthDegraded
		writer["save_timeouts"] = t
		status = healthDegraded
	}
	checks["message_writer"] = writer

	// Backups are only judged when enabled: switching them off is an
	// operator's choice, not a fault, though the fact is still reported.
	b := backupState.snapshot()
	backup := map[string]any{}
	switch {
	case !backupEnabled():
		backup["status"] = "disabled"
	case b["last_run"] == nil:
		// Enabled but never run yet: normal right after a restart,
		// worth a look if it persists.
		backup["status"] = healthOK
		backup["note"] = "has not run yet"
	default:
		backup["status"] = healthOK
		if ok, _ := b["last_ok"].(bool); !ok {
			backup["status"] = healthDegraded
			backup["error"] = b["last_error"]
			status = healthDegraded
		}
		// Staleness allows two intervals of slack so a single skipped
		// schedule does not raise an alarm.
		if age, okAge := b["age_hours"].(int); okAge {
			backup["age_hours"] = age
			if age > backupIntervalHours()*2 {
				backup["status"] = healthDegraded
				backup["stale"] = true
				status = healthDegraded
			}
		}
		if ok, present := b["offsite_ok"].(bool); present && !ok {
			// A local copy exists but the off-site one failed: survives a
			// disk failure, not the loss of the machine.
			backup["status"] = healthDegraded
			backup["offsite_failed"] = true
			status = healthDegraded
		}
	}
	checks["backup"] = backup

	return checks, status
}
