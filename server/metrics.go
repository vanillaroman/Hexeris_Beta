package main

// Lightweight observability with no external dependencies: atomic counters
// since process start, plus /healthz. A Prometheus client is a deliberate
// non-choice — it would add a dependency and a second metrics format, while
// /admin/metrics already serves JSON that anything can scrape.

import (
	"context"
	"encoding/json"
	"log"
	"net"
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
		"client":         clientView(r),
	})
}

// clientView — what the server sees about the caller, and why exactly that.
//
// "Everyone is 127.0.0.1 in the sign-in log" is a question not about the log
// but about which headers reach the application. Checking that by trying
// configurations on two machines is slow and unreliable: it is easy to edit
// the wrong location, the wrong file or the wrong machine, and there is no
// feedback — the log fills up some time later. Here the answer comes at once
// and from the very device it matters from: open /healthz?v=1 on a phone and
//
// look at ip. Nothing leaves beyond what the caller already knows: their own
// address and their own headers. Claiming a different address through them is
// impossible anyway — headers are believed only from trusted proxies (see
// getIP), and the trust flag is shown right there.
func clientView(r *http.Request) map[string]any {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	xff := r.Header.Get("X-Forwarded-For")
	realIP := r.Header.Get("X-Real-IP")
	trusted := trustedProxyIPs[host]

	view := map[string]any{
		"ip":              getIP(r), // <- this is what lands in the sign-in log
		"remote_addr":     host,
		"x_forwarded_for": xff,
		"x_real_ip":       realIP,
		"proxy_trusted":   trusted,
	}

	switch {
	case trusted && xff == "" && realIP == "":
		view["note"] = "connection comes from a trusted proxy, but it passes no client IP — " +
			"add 'proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;' to the nginx " +
			"location that proxies THIS app, then reload nginx"
	case !trusted && (xff != "" || realIP != ""):
		view["note"] = "forwarding headers are present but ignored: " + host +
			" is not a trusted proxy — add it to TRUSTED_PROXY_IPS if it really is one"
	case !trusted:
		view["note"] = "direct connection, no proxy involved"
	default:
		view["note"] = "client IP resolved from forwarding headers"
	}
	return view
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
