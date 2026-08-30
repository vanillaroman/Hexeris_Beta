package main

// Uploading and serving media files.
// Part of the Hexeris server; the shared types and main() live in main.go.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ─── /upload & /files ──────────────────────────────────────────────────────────

// allowedExts CLASSIFIES known extensions into a media_type. ANY type may be
// uploaded (unknown ones become "document", see uploadHandler); this map only
// decides what to show inline (image/video) and what to force as a download
// (document).
//
// IMPORTANT (security): ONLY raster images and video are served inline on our
// origin. Everything scriptable (svg, html) is kept as "document" — then
// filesHandler sets Content-Disposition: attachment and the file is downloaded
// rather than rendered (otherwise <script> in an svg or html is a stored XSS).
var allowedExts = map[string]string{
	".jpg": "image", ".jpeg": "image", ".png": "image",
	".gif": "image", ".webp": "image", ".heic": "image", ".heif": "image",
	".mp4": "video", ".mov": "video", ".webm": "video",
	".pdf": "document", ".txt": "document",
	".zip": "document", ".doc": "document", ".docx": "document",
	".xls": "document", ".xlsx": "document",
	// Audio from the composer "+" menu. Served as a file attachment (the same
	// path as documents) — no separate player is needed in the client.
	".mp3": "document", ".m4a": "document", ".ogg": "document",
	".wav": "document", ".aac": "document",
	// Scriptable formats EXPLICITLY as document (downloaded, not rendered).
	".svg": "document", ".html": "document", ".htm": "document",
	".xml": "document", ".xhtml": "document",
}

// safeExt — an extension of the form ".abc123" (letters/digits, 1–12
// characters). Anything else (odd names, no extension) is stored without one —
// the file is served as an attachment with application/octet-stream + nosniff.
var extRe = regexp.MustCompile(`^\.[a-z0-9]{1,12}$`)

func safeExt(ext string) string {
	if extRe.MatchString(ext) {
		return ext
	}
	return ""
}

// imageExts are validated against their magic bytes — anything else is
// accepted on extension alone (documents are not content-sniffed).
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true,
}

// imageMagic maps a file extension to a byte prefix that must appear at
// the start of the file. Prevents disguising executables as images.
var imageMagic = map[string][]byte{
	".jpg":  {0xFF, 0xD8, 0xFF},
	".jpeg": {0xFF, 0xD8, 0xFF},
	".png":  {0x89, 0x50, 0x4E, 0x47},
	".gif":  {0x47, 0x49, 0x46, 0x38},
	".webp": {0x52, 0x49, 0x46, 0x46},
}

// maxUploadBytes — a hard ceiling on the request body. ParseMultipartForm
// bounds only the in-memory buffer; anything above it went to disk, so a
// single 10 GB request filled the partition. MaxBytesReader cuts the connection.
const maxUploadBytes = 60 << 20 // 60 MB

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	username, ok := validateToken(extractToken(r))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if uploadLimiter.isBlocked(username) {
		http.Error(w, "too many uploads, try again later", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	// A 32 MB in-memory cap; the rest goes to a temporary file, but no more
	// than maxUploadBytes in total (MaxBytesReader cuts it off).
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "file too large (max 60 MB)", http.StatusRequestEntityTooLarge)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := safeExt(strings.ToLower(filepath.Ext(header.Filename)))
	// Any type is allowed: known ones are classified (image/video → inline,
	// everything else → document), unknown ones are accepted as a plain attachment.
	mediaType, ok := allowedExts[ext]
	if !ok {
		mediaType = "document"
	}

	// For images: read first 12 bytes and validate magic bytes.
	// This prevents an attacker from uploading a JS/HTML/ELF file
	// renamed as .png and having it served with a trusted URL.
	if imageExts[ext] {
		magic := make([]byte, 12)
		n, _ := file.Read(magic)
		expected, hasMagic := imageMagic[ext]
		if hasMagic && (n < len(expected) || !bytes.HasPrefix(magic[:n], expected)) {
			http.Error(w, "file content does not match extension", http.StatusBadRequest)
			return
		}
		// Seek back so io.Copy gets the full file
		if seeker, ok := file.(io.Seeker); ok {
			seeker.Seek(0, io.SeekStart)
		}
	}

	os.MkdirAll(uploadDir(), 0o755)
	filename := randomFileName(ext)
	dst, err := os.Create(filepath.Join(uploadDir(), filename))
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer dst.Close()
	// Encrypted on the fly: what reaches the disk is [magic][IV][AES-256-CTR
	// ciphertext], not the original bytes. The key is the shared SERVER_ENC_KEY.
	if err := encryptStream(dst, file); err != nil {
		os.Remove(filepath.Join(uploadDir(), filename)) // leave no stub behind
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	uploadLimiter.recordFailure(username) // every upload is counted

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"url":        "/files/" + filename,
		"media_type": mediaType,
	})
}

// randomFileName — 128 bits from crypto/rand: the names are unpredictable, and
// knowing the upload time no longer allows guessing a URL. The old format
// (UnixNano with math/rand) stays valid for files that already exist.
func randomFileName(ext string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Practically unreachable; better to die than issue a predictable name.
		log.Fatal("crypto/rand failed:", err)
	}
	return hex.EncodeToString(b) + ext
}

// sanitizeHeaderFilename keeps only the alphabet that is safe in an HTTP
// header; everything else becomes "_". An empty result becomes "file".
func sanitizeHeaderFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}

// noStore forbids caching a REFUSAL.
//
// Error responses inherited the shared media Cache-Control
// (private, max-age=86400) — so the browser remembered a 404 or a 401 for a
// day and stopped asking the server. Meanwhile the problem was fixed on the
// server while the client kept showing the old error from its own cache.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func filesHandler(w http.ResponseWriter, r *http.Request) {
	// Authorisation: the cookie first (<img>/<video> send it automatically), then
	// a Bearer or query token as a fallback for API clients.
	//
	// The cookie here is a necessity rather than a convenience: <img
	// src="/files/…"> and <video> never send an Authorization header at all, so
	// media is authorised by it.
	tok := ""
	fromCookie := false
	if c, err := r.Cookie(authCookieName); err == nil {
		tok, fromCookie = c.Value, true
	}
	if tok == "" {
		tok = extractToken(r)
	}
	if _, ok := validateToken(tok); !ok {
		// The log records WHY it was refused. An "unauthorized" with no
		// explanation cannot separate three completely different situations: the
		// request arrived with no credentials at all (usually a curl check), the
		// cookie is there but the token expired or was signed with a different
		// JWT_SECRET, or a token was passed but is invalid. All three used to look
		// the same and the investigation ran on guesses. The token itself is of
		// course never written.
		switch {
		case tok == "":
			log.Printf("files: refused — request with no %s cookie and no token (%s)", authCookieName, r.URL.Path)
		case fromCookie:
			log.Printf("files: refused — the %s cookie is present but the token fails validation "+
				"(expired, revoked, or JWT_SECRET changed)", authCookieName)
		default:
			log.Printf("files: refused — the supplied token fails validation")
		}
		noStore(w)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	filename := filepath.Base(r.URL.Path)
	if filename == "." || filename == "/" || strings.Contains(filename, "..") {
		noStore(w)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// private: media must no longer settle in shared caches or proxies.
	//
	// MIND THE ORDER. The header is set BEFORE it is known whether the file can
	// be served, so every refusal branch below MUST replace it with no-store
	// (see noStore). Otherwise the browser remembers the error itself for a day:
	// one 404 and the client stops asking the server entirely, showing its own
	// cache. From outside that looks like "the server is silent", the log is
	// empty, and the investigation reaches a dead end.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Images and video are served inline (otherwise viewing in the chat breaks),
	// everything else as a forced download: the browser will not render a pdf,
	// doc or zip in the context of our origin.
	ext := strings.ToLower(filepath.Ext(filename))
	if mt, ok := allowedExts[ext]; !ok || (mt != "image" && mt != "video") {
		// The name comes from the URL, i.e. it is under the client's control: a
		// quote in it would break the quotes of the header itself. Real names are
		// hex+ext (randomFileName), so everything outside that alphabet is stripped
		// rather than escaped. The real file name is carried by the client in the
		// link #fragment and never reaches the server.
		w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeHeaderFilename(filename)+"\"")
	}

	// serveUploadedFile decrypts the file as a stream (with Range support) or, if
	// it is a legacy unencrypted file, serves it as ordinary static content.
	serveUploadedFile(w, r, filepath.Join(uploadDir(), filename), ext)
}
