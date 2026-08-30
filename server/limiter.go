package main

// Rate limiting and client IP resolution.

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type RateLimiter struct {
	attempts    map[string][]int64
	mu          sync.Mutex
	maxAttempts int
	windowSecs  int64
}

const defaultWindowSecs = 600 // 10 minutes, the default window everywhere here

func newLimiter(max int, windowSecs int64) *RateLimiter {
	return &RateLimiter{attempts: make(map[string][]int64), maxAttempts: max, windowSecs: windowSecs}
}

var loginLimiter = newLimiter(5, defaultWindowSecs) // sign-in attempts per IP

// loginUserLimiter is the second tier for /login, keyed by username: a
// distributed guess at one account's password is invisible to the IP limiter.
var loginUserLimiter = newLimiter(10, defaultWindowSecs)

// registerLimiter caps accounts created from one IP. The threshold is an env
// setting because where open registration is enabled on purpose (an internal
// stand, a training session) everyone shares one NAT address, and a low cap
// blocks the second colleague to sign up while still stopping scripted abuse.
var registerLimiter = newLimiter(getEnvInt("REGISTER_MAX_PER_IP", 3), defaultWindowSecs)
var registerUserLimiter = newLimiter(3, defaultWindowSecs)

// Limits on expensive operations are keyed by username rather than IP: many
// users share one NAT address, and abuse always belongs to an account.
var uploadLimiter = newLimiter(30, defaultWindowSecs)

// Search scans up to 20k messages and decrypts each one.
var searchLimiter = newLimiter(60, defaultWindowSecs)

// statusLimiter throttles username enumeration through /status. 200 requests
// per window is ample for polling one's own contacts but makes scanning the
// user base impractical.
var statusLimiter = newLimiter(200, defaultWindowSecs)

// The heaviest GET endpoints: /history without a peer scans every
// conversation and decrypts each body, /reactions joins over the caller's
// messages. A legitimate client calls each once per reconnect plus paging,
// so 300 per window covers even a very unstable mobile network.
var historyLimiter = newLimiter(300, defaultWindowSecs)
var reactionsSyncLimiter = newLimiter(300, defaultWindowSecs)

// Message edit/delete. Unlimited, /edit-message is a free channel for bulk
// UPDATEs plus a broadcast to every device of the peer.
var msgMutateLimiter = newLimiter(80, defaultWindowSecs)

// Reaction toggles over WS: the only WS message type that writes to the
// database and broadcasts.
var reactionLimiter = newLimiter(120, defaultWindowSecs)

// Group creation leaves rows in two tables permanently.
var groupCreateLimiter = newLimiter(20, defaultWindowSecs)

// Lookups of other people's cards via /api/profile?user=. The endpoint
// answers for an arbitrary name (200 vs 404), so without a limit it is an
// account-existence oracle and a directory scanner. Own profile is exempt.
var profileLookupLimiter = newLimiter(200, defaultWindowSecs)

func (rl *RateLimiter) isBlocked(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now().Unix()
	cutoff := now - rl.windowSecs
	valid := []int64{}
	for _, t := range rl.attempts[key] {
		if t > cutoff {
			valid = append(valid, t)
		}
	}
	rl.attempts[key] = valid
	return len(valid) >= rl.maxAttempts
}

func (rl *RateLimiter) recordFailure(key string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.attempts[key] = append(rl.attempts[key], time.Now().Unix())
}

// cleanup removes keys whose entries have all aged out. Without it the map
// keeps one entry per IP that ever touched login/register — a slow but
// certain memory leak.
func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Unix() - rl.windowSecs
	for key, times := range rl.attempts {
		alive := times[:0]
		for _, t := range times {
			if t > cutoff {
				alive = append(alive, t)
			}
		}
		if len(alive) == 0 {
			delete(rl.attempts, key)
		} else {
			rl.attempts[key] = alive
		}
	}
}

func startLimiterJanitor() {
	safeGo("limiterJanitor", func() {
		for range time.Tick(10 * time.Minute) {
			loginLimiter.cleanup()
			registerLimiter.cleanup()
			registerUserLimiter.cleanup()
			uploadLimiter.cleanup()
			searchLimiter.cleanup()
			statusLimiter.cleanup()
			msgMutateLimiter.cleanup()
			reactionLimiter.cleanup()
			loginUserLimiter.cleanup()
			historyLimiter.cleanup()
			reactionsSyncLimiter.cleanup()
			groupCreateLimiter.cleanup()
			profileLookupLimiter.cleanup()
			googleAuthLimiter.cleanup()
			cleanupCallQueues()
		}
	})
}

// trustedProxyIPs lists the addresses whose X-Forwarded-For / X-Real-IP
// headers are trusted. From anywhere else those headers are ignored, or any
// client could forge one and bypass rate limits and ADMIN_ALLOWED_IPS.
// Loopback is always trusted, since a reverse proxy on the same host is the
// common case; add others via TRUSTED_PROXY_IPS.
var trustedProxyIPs = func() map[string]bool {
	out := map[string]bool{"127.0.0.1": true, "::1": true}
	for _, ip := range strings.Split(os.Getenv("TRUSTED_PROXY_IPS"), ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			out[ip] = true
		}
	}
	return out
}()

func getIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !trustedProxyIPs[ip] {
		return ip
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if real := strings.TrimSpace(strings.Split(fwd, ",")[0]); real != "" {
			return real
		}
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	return ip
}

// /google-auth is unauthenticated and calls out to Google on every request
// (plus bcrypt when it creates an account), so unlimited it is a load
// amplifier. The threshold is higher than /login because legitimate retries
// land here and password guessing is impossible: Google verifies the token.
var googleAuthLimiter = newLimiter(30, defaultWindowSecs)
