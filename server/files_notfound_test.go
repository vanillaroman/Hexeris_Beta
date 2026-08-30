package main

// Serving files: "no such file" and "could not read it" are DIFFERENT answers.
//
// The check appeared after a real dead end on a production server: the browser
// showed a 404 for a picture that was physically in the directory. The cause
// was not a missing file, but the 404 sent people looking for exactly that.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeUploadedFileDistinguishesMissingFromUnreadable(t *testing.T) {
	dir := t.TempDir()

	// 1. No file — an honest 404.
	w := httptest.NewRecorder()
	serveUploadedFile(w, httptest.NewRequest(http.MethodGet, "/files/nope.gif", nil),
		filepath.Join(dir, "nope.gif"), ".gif")
	if w.Code != http.StatusNotFound {
		t.Errorf("missing file: got %d, expected 404", w.Code)
	}

	// 2. The file exists but cannot be opened. Permissions do not work under
	//    root (the tests often run as root, and mode 000 is no obstacle to it),
	//    so the failure is reproduced differently: an ordinary file where a
	//    DIRECTORY should be in the path. The kernel answers ENOTDIR — an error
	//    that is not "does not exist".
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	w = httptest.NewRecorder()
	serveUploadedFile(w, httptest.NewRequest(http.MethodGet, "/files/x.gif", nil),
		filepath.Join(blocker, "x.gif"), ".gif")
	if w.Code == http.StatusNotFound {
		t.Error("an unreadable file was served as a 404 — the administrator will hunt for a missing file instead of the real cause")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("unreadable file: got %d, expected 500", w.Code)
	}

	// 3. A control: a real file is served. Otherwise the checks above would pass
	//    for a function that can do nothing at all.
	good := filepath.Join(dir, "good.gif")
	if err := os.WriteFile(good, []byte("GIF89a-not-encrypted"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	w = httptest.NewRecorder()
	serveUploadedFile(w, httptest.NewRequest(http.MethodGet, "/files/good.gif", nil), good, ".gif")
	if w.Code != http.StatusOK {
		t.Errorf("existing file: got %d, expected 200", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("the body is empty — the file was not served")
	}
}

// The three media authorisation paths. The check appeared after an
// investigation where curl without a cookie returned 401 and that was taken
// for a fault: it is exactly right, and a browser goes with the cookie.
//
// The cookie here is no convenience: <img src="/files/…"> and <video> never
// send an Authorization header at all.
func TestFilesHandlerAuthPaths(t *testing.T) {
	if os.Getenv("JWT_SECRET") == "" {
		os.Setenv("JWT_SECRET", "test-secret-for-files-auth-paths-0123456789")
	}
	// filesHandler calls uploadDir(), which stops the process when UPLOAD_DIR is
	// empty — that is the production behaviour, and changing it for a test is
	// not an option. So a directory is provided.
	if os.Getenv("UPLOAD_DIR") == "" {
		os.Setenv("UPLOAD_DIR", t.TempDir())
	}
	tok, err := generateToken("mediauser")
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	call := func(mut func(*http.Request)) int {
		r := httptest.NewRequest(http.MethodGet, "/files/whatever.gif", nil)
		if mut != nil {
			mut(r)
		}
		w := httptest.NewRecorder()
		filesHandler(w, r)
		return w.Code
	}

	// With no credentials, a 401. That is what curl shows, and it is correct.
	if code := call(nil); code != http.StatusUnauthorized {
		t.Errorf("no cookie and no token: got %d, expected 401", code)
	}
	// A rubbish cookie does not pass either.
	if code := call(func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: "not-a-jwt"})
	}); code != http.StatusUnauthorized {
		t.Errorf("invalid cookie: got %d, expected 401", code)
	}

	// With a real cookie the request passes authorisation and reaches the file:
	// there is no file, hence a 404 — but that is NO LONGER an access refusal.
	if code := call(func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: tok})
	}); code != http.StatusNotFound {
		t.Errorf("valid cookie: got %d, expected 404 (authorisation passed)", code)
	}
	// Bearer — for API clients.
	if code := call(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tok)
	}); code != http.StatusNotFound {
		t.Errorf("Bearer: got %d, expected 404", code)
	}
	// A query token — the fallback where a header cannot be set.
	if code := call(func(r *http.Request) {
		r.URL.RawQuery = "token=" + tok
	}); code != http.StatusNotFound {
		t.Errorf("query token: got %d, expected 404", code)
	}
}

// describeDirFor must tell three states apart, because the "the file is there
// / there is no file" argument is settled by them: the directory is visible
// with files, the directory is visible but empty (different visibility — a
// systemd sandbox), or there is no directory at all.
func TestDescribeDirFor(t *testing.T) {
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "a.gif"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := describeDirFor(filepath.Join(full, "missing.gif"))
	if !strings.Contains(got, "a.gif") {
		t.Errorf("a directory with files must list them: %q", got)
	}
	if strings.Contains(got, "EMPTY") {
		t.Errorf("a non-empty directory was called empty: %q", got)
	}

	// An empty directory is the main signal: the file is being looked for where
	// the process sees nothing at all.
	empty := t.TempDir()
	got = describeDirFor(filepath.Join(empty, "missing.gif"))
	if !strings.Contains(got, "EMPTY") {
		t.Errorf("an empty directory was not flagged: %q", got)
	}
	if !strings.Contains(got, "ProtectHome") {
		t.Errorf("the likely cause of the discrepancy is not named: %q", got)
	}

	// No directory is its own case too, not "empty".
	got = describeDirFor(filepath.Join(empty, "nosuchdir", "x.gif"))
	if !strings.Contains(got, "cannot access") {
		t.Errorf("a missing directory was not flagged: %q", got)
	}
}

// An ERROR response must not be cached.
//
// Because of the shared media Cache-Control (private, max-age=86400) the
// browser remembered a 404 or a 401 for a day and stopped asking the server.
// From outside that is indistinguishable from "the server is silent": the
// problem has been fixed on the server while the client shows the old error
// from its cache, and the log is empty — because there is no request.
func TestFilesErrorsAreNotCacheable(t *testing.T) {
	if os.Getenv("JWT_SECRET") == "" {
		os.Setenv("JWT_SECRET", "test-secret-for-files-cache-headers-0123456789")
	}
	if os.Getenv("UPLOAD_DIR") == "" {
		os.Setenv("UPLOAD_DIR", t.TempDir())
	}
	tok, err := generateToken("cacheuser")
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	cacheable := func(h http.Header) bool {
		cc := h.Get("Cache-Control")
		return cc != "" && !strings.Contains(cc, "no-store")
	}

	// 401 — with no credentials.
	w := httptest.NewRecorder()
	filesHandler(w, httptest.NewRequest(http.MethodGet, "/files/x.gif", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("setup: expected 401, got %d", w.Code)
	}
	if cacheable(w.Header()) {
		t.Errorf("the 401 is cacheable: %q — the browser will remember the refusal and stop asking the server",
			w.Header().Get("Cache-Control"))
	}

	// 404 — authorisation passed, no file.
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/files/nosuch.gif", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: tok})
	filesHandler(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("setup: expected 404, got %d", w.Code)
	}
	if cacheable(w.Header()) {
		t.Errorf("the 404 is cacheable: %q — that is exactly how the error stuck for a day",
			w.Header().Get("Cache-Control"))
	}

	// A control: a SUCCESSFUL response must stay cacheable, otherwise every
	// picture in a conversation would be fetched again and one trouble would be
	// swapped for another. uploadDir() rather than the environment variable: the
	// value is cached through sync.OnceValue, and if an earlier test fixed the
	// directory, the file must go where the handler actually looks. That
	// directory may already have been cleaned up by that test — recreate it.
	if err := os.MkdirAll(uploadDir(), 0o700); err != nil {
		t.Fatalf("preparing the directory: %v", err)
	}
	good := filepath.Join(uploadDir(), "good.gif")
	if err := os.WriteFile(good, []byte("GIF89a-plain"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/files/good.gif", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: tok})
	filesHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("successful serve: got %d", w.Code)
	}
	if !cacheable(w.Header()) {
		t.Errorf("a successful response stopped being cacheable: %q", w.Header().Get("Cache-Control"))
	}
}
