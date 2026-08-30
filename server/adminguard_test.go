package main

// Rejections at the admin boundary must be distinguishable.
//
// Answering "forbidden" to both a wrong key and an unlisted address leaves an
// operator with one identical 403 and two different places to fix blindly.
//
// The line between explaining and staying silent is deliberate: the reason is
// disclosed only to someone who already presented a valid key.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The fingerprint must answer the question it exists for: are two keys of
// equal length the same key or not? Length cannot answer it — the same
// generator run twice yields two keys of identical length.
func TestKeyFingerprintSeparatesEqualLengthKeys(t *testing.T) {
	a := strings.Repeat("a", 64)
	b := strings.Repeat("b", 64)
	if len(a) != len(b) {
		t.Fatal("the keys under test must be the same length")
	}
	if keyFingerprint(a) == keyFingerprint(b) {
		t.Fatal("different keys of equal length share a fingerprint")
	}
	// Control: the same key must fingerprint identically, or comparing
	// entries in a log would be meaningless.
	if keyFingerprint(a) != keyFingerprint(a) {
		t.Fatal("fingerprint is not stable")
	}
	// Neither the key nor any part of it may reach the log.
	fp := keyFingerprint(a)
	if len(fp) != 8 {
		t.Fatalf("fingerprint length %d, want 8", len(fp))
	}
	if strings.Contains(a, fp) {
		t.Fatalf("fingerprint %q occurs inside the key itself", fp)
	}
	// A missing header is its own diagnosis, not "some key".
	if keyFingerprint("") == keyFingerprint("x") {
		t.Fatal("an empty key is indistinguishable from a non-empty one")
	}
	if fp := keyFingerprint(""); !strings.Contains(fp, "empty") {
		t.Fatalf("empty key labelled %q — reads like an ordinary fingerprint", fp)
	}
}

func TestAdminGuardRejectionsAreDistinguishable(t *testing.T) {
	// adminGuard sets CORS from cfg, which main() fills in production and
	// this unit test must provide.
	origCfg := cfg
	if cfg == nil {
		cfg = loadConfig()
	}
	origKey, origIPs := adminKey, adminAllowedIPs
	t.Cleanup(func() { adminKey, adminAllowedIPs, cfg = origKey, origIPs, origCfg })

	adminKey = func() string { return "correct-key" }
	adminAllowedIPs = map[string]bool{"198.51.100.7": true}

	call := func(key, remote string) (int, string) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin/metrics", nil)
		req.Header.Set("X-Admin-Key", key)
		req.RemoteAddr = remote + ":1234"
		adminGuard(rr, req)
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}

	// Wrong key: refused without detail.
	code, body := call("wrong", "198.51.100.7")
	if code != http.StatusForbidden {
		t.Fatalf("wrong key: want 403, got %d", code)
	}
	if strings.Contains(body, "ADMIN_ALLOWED_IPS") || strings.Contains(body, "198.51.100.7") {
		t.Fatalf("key rejection leaked how the guard works: %q", body)
	}
	if strings.Contains(body, "correct-key") {
		t.Fatalf("the key itself appeared in the response: %q", body)
	}

	// Valid key from an unlisted address: the reason is stated.
	code, ipBody := call("correct-key", "203.0.113.9")
	if code != http.StatusForbidden {
		t.Fatalf("unlisted address: want 403, got %d", code)
	}
	if !strings.Contains(ipBody, "203.0.113.9") || !strings.Contains(ipBody, "ADMIN_ALLOWED_IPS") {
		t.Fatalf("address rejection does not state a reason: %q", ipBody)
	}

	// The point: the two rejections no longer look alike.
	if body == ipBody {
		t.Fatalf("both rejections answer %q — indistinguishable again", body)
	}

	// Control: a valid key from an allowed address passes. Without it the
	// test would also pass against a guard that refuses everything.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/metrics", nil)
	req.Header.Set("X-Admin-Key", "correct-key")
	req.RemoteAddr = "198.51.100.7:1234"
	if !adminGuard(rr, req) {
		t.Fatalf("valid key from an allowed address was refused: %d %s", rr.Code, rr.Body.String())
	}

	// An empty ADMIN_ALLOWED_IPS means the filter is off, not that nobody
	// is allowed.
	adminAllowedIPs = map[string]bool{}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/metrics", nil)
	req.Header.Set("X-Admin-Key", "correct-key")
	req.RemoteAddr = "203.0.113.9:1234"
	if !adminGuard(rr, req) {
		t.Fatal("with the IP filter off a valid key must pass")
	}
}
