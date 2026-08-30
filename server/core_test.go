package main

// Unit tests for critical logic without a database or network: body
// encryption, JWT, parsing helpers, the SSRF IP filter, client IP resolution
// behind a proxy and the rate limiter.

import (
	"bytes"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func stubKeys(t *testing.T) {
	t.Helper()
	origEnc, origJwt := encKey, jwtSecret
	encKey = func() []byte { return bytes.Repeat([]byte{0x24}, 32) }
	jwtSecret = func() string { return "test-secret" }
	t.Cleanup(func() { encKey, jwtSecret = origEnc, origJwt })
}

func TestEncryptDecryptBodyRoundTrip(t *testing.T) {
	stubKeys(t)
	for _, body := range []string{"", "hi", "héllo, wörld 🚀", strings.Repeat("x", 5000)} {
		enc := encryptBody(body)
		if body != "" && enc == body {
			t.Fatalf("encryptBody returned plaintext for %q", body)
		}
		if got := decryptBody(enc); got != body {
			t.Fatalf("roundtrip: got %q want %q", got, body)
		}
	}
}

func TestDecryptBodyPassthrough(t *testing.T) {
	stubKeys(t)
	// Legacy rows that are not ciphertext must come back unchanged rather
	// than as garbage: plaintext media URLs rely on it.
	for _, s := range []string{"/files/abc.png", "not-encrypted at all!", "AAAA"} {
		if got := decryptBody(s); got != s {
			t.Fatalf("passthrough broken: %q -> %q", s, got)
		}
	}
	// Damaged ciphertext (valid base64, broken GCM tag) passes through too.
	enc := encryptBody("secret")
	tampered := enc[:len(enc)-4] + "AAA="
	if got := decryptBody(tampered); got == "secret" {
		t.Fatal("tampered ciphertext must not decrypt to plaintext")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	stubKeys(t)
	tok, err := generateToken("alice")
	if err != nil {
		t.Fatal(err)
	}
	user, ok := validateToken(tok)
	if !ok || user != "alice" {
		t.Fatalf("validateToken: got (%q,%v)", user, ok)
	}
	if _, ok := validateToken("garbage.token.here"); ok {
		t.Fatal("garbage token accepted")
	}
	if _, ok := validateToken(""); ok {
		t.Fatal("empty token accepted")
	}
	// A token issued before the logout cutoff must stop working.
	setLogoutCutoff("alice", time.Now().Unix()+1)
	if _, ok := validateToken(tok); ok {
		t.Fatal("token issued before logout cutoff accepted")
	}
	logoutCutoffs.Delete("alice")
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Hexeris":    "hexeris",
		"My App 2.0": "my-app-2-0",
		"ABC":        "abc",
	} {
		if got := slugify(in); got != want {
			t.Fatalf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitTrim(t *testing.T) {
	got := splitTrim(" a, b ,,c ", ",")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitTrim: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitTrim[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMakeSnippet(t *testing.T) {
	if got := makeSnippet("short body", "short"); got != "short body" {
		t.Fatalf("short body must be returned whole, got %q", got)
	}
	long := strings.Repeat("a", 100) + " NEEDLE " + strings.Repeat("b", 100)
	got := makeSnippet(long, "needle")
	if !strings.Contains(got, "NEEDLE") {
		t.Fatalf("snippet lost the match: %q", got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("mid-string snippet must be ellipsed on both sides: %q", got)
	}
}

func TestIsDisallowedIP(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.1.2.3", "192.168.0.5", "172.16.0.1",
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "0.0.0.0", "::1", "fe80::1"}
	for _, s := range blocked {
		if !isDisallowedIP(net.ParseIP(s)) {
			t.Fatalf("%s must be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "100.63.0.1", "100.128.0.1", "2606:4700::1111"}
	for _, s := range allowed {
		if isDisallowedIP(net.ParseIP(s)) {
			t.Fatalf("%s must be allowed", s)
		}
	}
	if !isDisallowedIP(nil) {
		t.Fatal("nil IP must be blocked")
	}
}

func TestGetIPTrustedProxy(t *testing.T) {
	// Headers from an untrusted address are ignored.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7:1234"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if got := getIP(r); got != "203.0.113.7" {
		t.Fatalf("untrusted XFF must be ignored, got %q", got)
	}
	// Loopback is always trusted, so XFF is honoured.
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "127.0.0.1:80"
	r2.Header.Set("X-Forwarded-For", "9.9.9.9, 10.0.0.1")
	if got := getIP(r2); got != "9.9.9.9" {
		t.Fatalf("trusted proxy XFF: got %q, want 9.9.9.9", got)
	}
}

func TestDBDSNStatementTimeout(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@localhost/db":            "postgres://u:p@localhost/db?statement_timeout=10000&connect_timeout=3",
		"postgres://u:p@localhost/db?sslmode=on": "postgres://u:p@localhost/db?sslmode=on&statement_timeout=10000&connect_timeout=3",
		"host=localhost dbname=db":               "host=localhost dbname=db statement_timeout=10000 connect_timeout=3",
		// Already present in the DSN: neither is touched.
		"host=x statement_timeout=500":                   "host=x statement_timeout=500 connect_timeout=3",
		"host=x statement_timeout=500 connect_timeout=9": "host=x statement_timeout=500 connect_timeout=9",
	}
	for in, want := range cases {
		t.Setenv("DATABASE_URL", in)
		if got := dbDSN(); got != want {
			t.Fatalf("dbDSN(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("DATABASE_URL", "host=localhost")
	t.Setenv("DB_STATEMENT_TIMEOUT_MS", "5000")
	t.Setenv("DB_CONNECT_TIMEOUT_S", "7")
	if got := dbDSN(); got != "host=localhost statement_timeout=5000 connect_timeout=7" {
		t.Fatalf("env override broken: %q", got)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := newLimiter(3, 600)
	key := "k"
	for i := 0; i < 3; i++ {
		if rl.isBlocked(key) {
			t.Fatalf("blocked too early at attempt %d", i)
		}
		rl.recordFailure(key)
	}
	if !rl.isBlocked(key) {
		t.Fatal("must be blocked after max attempts")
	}
	// Expired entries are removed entirely.
	rl.attempts[key] = []int64{time.Now().Unix() - 700}
	rl.cleanup()
	if _, exists := rl.attempts[key]; exists {
		t.Fatal("cleanup must drop fully-expired keys")
	}
}
