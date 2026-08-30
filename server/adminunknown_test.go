package main

// An unknown admin endpoint must answer 404 — and that 404 must be
// recognisable.
//
// The breakage this file was written for was diagnostic rather than
// functional, and it cost more than usual. The panel showed "Endpoint not
// found (404). The server is likely running an older build — deploy the
// latest and restart it." The server was rebuilt and restarted; nothing
// changed.
//
// The cause: /admin/* without its own handler fell through to the "/"
// catch-all and got the MESSENGER's index.html with a 200. A 404 from the
// server for a missing endpoint never arrived AT ALL, and every 404 the panel
// saw came from nginx on admin.example.com without ever reaching the
// /admin-api/ location. The hint confidently pointed at the wrong machine.
//
// Hence two checks: the server returns a recognisable 404, and the panel looks
// for exactly the substring the server sends.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withAdminKey(t *testing.T) {
	t.Helper()
	origCfg := cfg
	if cfg == nil {
		cfg = loadConfig()
	}
	origKey, origIPs := adminKey, adminAllowedIPs
	t.Cleanup(func() { adminKey, adminAllowedIPs, cfg = origKey, origIPs, origCfg })
	adminKey = func() string { return "correct-key" }
	adminAllowedIPs = map[string]bool{"198.51.100.7": true}
}

// TestUnknownAdminEndpointIs404 — the main invariant: a non-existent endpoint
// answers 404, not 200 with the messenger's page.
func TestUnknownAdminEndpointIs404(t *testing.T) {
	withAdminKey(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/no-such-thing", nil)
	req.Header.Set("X-Admin-Key", "correct-key")
	req.RemoteAddr = "198.51.100.7:1234"
	adminUnknownHandler(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("an unknown endpoint answered %d, expected 404", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, adminUnknownEndpointMarker) {
		t.Fatalf("the body carries no %q marker: %q", adminUnknownEndpointMarker, body)
	}
	// The body must be short and non-HTML: the panel puts it into the hint only
	// when it is under 200 characters and does not start with "<".
	if len(strings.TrimSpace(body)) >= 200 || strings.HasPrefix(strings.TrimSpace(body), "<") {
		t.Fatalf("the panel will not put such a body into the hint: %q", body)
	}
}

// TestUnknownAdminEndpointNeedsKey — the 404 must not enumerate endpoints for
// anyone who did not present a key.
func TestUnknownAdminEndpointNeedsKey(t *testing.T) {
	withAdminKey(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/no-such-thing", nil)
	req.Header.Set("X-Admin-Key", "wrong")
	req.RemoteAddr = "198.51.100.7:1234"
	adminUnknownHandler(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("without a valid key the answer is %d, expected 403", rr.Code)
	}
	if strings.Contains(rr.Body.String(), adminUnknownEndpointMarker) {
		t.Fatal("the key refusal revealed that no such endpoint exists")
	}
}

// TestAdminCatchAllDoesNotShadowRealRoutes — a negative control for the fix.
// Registering the "/admin/" subtree must not intercept exact routes:
// otherwise the whole panel would get a 404 and the cure would beat the disease.
func TestAdminCatchAllDoesNotShadowRealRoutes(t *testing.T) {
	mux := http.NewServeMux()
	hit := ""
	mux.HandleFunc("/admin/metrics", func(http.ResponseWriter, *http.Request) { hit = "metrics" })
	mux.HandleFunc("/admin/", func(http.ResponseWriter, *http.Request) { hit = "catch-all" })

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/metrics", nil))
	if hit != "metrics" {
		t.Fatalf("an exact route was intercepted by the subtree: %q fired", hit)
	}
	hit = ""
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/admin/nope", nil))
	if hit != "catch-all" {
		t.Fatalf("an unknown route did not reach the subtree: %q fired", hit)
	}
}

// TestPanelMatchesServerMarker — a contract between two files that live in
// different directories and are edited separately. If the marker is renamed on
// the server and forgotten in the panel, the panel returns to its old wrong
// hint about an "older build", and nothing would catch that by eye.
func TestPanelMatchesServerMarker(t *testing.T) {
	path := filepath.Join("..", "docs", "admin-panel", "admin-index.html")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("the panel was not found (%v) — the contract check is skipped", err)
	}
	if !strings.Contains(string(b), adminUnknownEndpointMarker) {
		t.Fatalf("the panel does not look for the %q marker — a 404 from the messenger "+
			"and one from nginx are indistinguishable again", adminUnknownEndpointMarker)
	}
}

// TestAdminAPIAliasRoutesToRealHandlers — the prefix the panel uses is served
// by the same set of handlers.
//
// The point of the change is that the proxy must NOT rewrite the path: a
// capture-group substitution on that side drifted in production and took the
// whole panel with it. What is checked is that the path is rewritten here and
// lands in exactly the same place, and that the mux cannot loop on itself.
func TestAdminAPIAliasRoutesToRealHandlers(t *testing.T) {
	mux := http.NewServeMux()
	hit := ""
	mux.HandleFunc("/admin/metrics", func(http.ResponseWriter, *http.Request) { hit = "metrics" })
	mux.HandleFunc("/admin/", func(http.ResponseWriter, *http.Request) { hit = "admin-catch-all" })
	mux.HandleFunc(adminAPIAliasPrefix, func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/admin/" + strings.TrimPrefix(r.URL.Path, adminAPIAliasPrefix)
		r2.URL.RawPath = ""
		mux.ServeHTTP(w, r2)
	})

	for _, c := range []struct{ path, want string }{
		{"/admin-api/metrics", "metrics"},
		{"/admin-api/nope", "admin-catch-all"},
		// A path that looks like an attempt to make the mux loop.
		{"/admin-api/admin-api/metrics", "admin-catch-all"},
		{"/admin-api/", "admin-catch-all"},
	} {
		hit = ""
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, c.path, nil))
		if hit != c.want {
			t.Fatalf("%s -> %q fired, expected %q", c.path, hit, c.want)
		}
	}

	// The query string must arrive untouched: pagination, filters and the
	// mandatory export parameters all rest on it.
	var gotQuery string
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/admin/users", func(_ http.ResponseWriter, r *http.Request) { gotQuery = r.URL.RawQuery })
	mux2.HandleFunc(adminAPIAliasPrefix, func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/admin/" + strings.TrimPrefix(r.URL.Path, adminAPIAliasPrefix)
		r2.URL.RawPath = ""
		mux2.ServeHTTP(w, r2)
	})
	mux2.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/admin-api/users?q=&limit=20&offset=0&filter=", nil))
	if gotQuery != "q=&limit=20&offset=0&filter=" {
		t.Fatalf("the query was lost while rewriting the path: %q", gotQuery)
	}
}
