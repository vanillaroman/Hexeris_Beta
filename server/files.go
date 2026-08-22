package main

// Upload and delivery of attachments.

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

// allowedExts classifies known extensions into a media_type. Any file type
// may be uploaded — unknown ones become "document" — so this map only decides
// what is shown inline and what is forced to download.
//
// Only raster images and video are served inline from this origin. Anything
// scriptable (svg, html) is deliberately classified as "document" so it is
// downloaded rather than rendered; otherwise a <script> inside an uploaded
// svg or html file is stored XSS.
var allowedExts = map[string]string{
	".jpg": "image", ".jpeg": "image", ".png": "image",
	".gif": "image", ".webp": "image", ".heic": "image", ".heif": "image",
	".mp4": "video", ".mov": "video", ".webm": "video",
	".pdf": "document", ".txt": "document",
	".zip": "document", ".doc": "document", ".docx": "document",
	".xls": "document", ".xlsx": "document",
	// Audio files travel the same path as documents; the client needs no
	// separate player for them.
	".mp3": "document", ".m4a": "document", ".ogg": "document",
	".wav": "document", ".aac": "document",
	// Scriptable formats, explicitly documents so they download.
	".svg": "document", ".html": "document", ".htm": "document",
	".xml": "document", ".xhtml": "document",
}

// Extensions are accepted only as ".abc123" (1–12 alphanumerics). Anything
// else is stored without an extension and still served as an attachment with
// application/octet-stream and nosniff.
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

// A hard ceiling on the request body. ParseMultipartForm bounds only the
// in-memory buffer and spills the rest to disk, so without MaxBytesReader a
// single huge request can fill the partition.
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
	// 32 MB in memory; the rest spills to a temporary file, still bounded
	// overall by MaxBytesReader above.
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
	// Any type is accepted; unknown ones are treated as plain attachments.
	mediaType, ok := allowedExts[ext]
	if !ok {
		mediaType = "document"
	}

	// Images are checked against their magic bytes, so a JS/HTML/ELF file
	// renamed to .png cannot be served from a trusted URL.
	if imageExts[ext] {
		magic := make([]byte, 12)
		n, _ := file.Read(magic)
		expected, hasMagic := imageMagic[ext]
		if hasMagic && (n < len(expected) || !bytes.HasPrefix(magic[:n], expected)) {
			http.Error(w, "file content does not match extension", http.StatusBadRequest)
			return
		}
		// Seek back so the copy below gets the full file.
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
	// Encrypted on the way in: the disk holds [magic][IV][ciphertext].
	if err := encryptStream(dst, file); err != nil {
		os.Remove(filepath.Join(uploadDir(), filename)) // no truncated leftovers
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	uploadLimiter.recordFailure(username) // every upload counts

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"url":        "/files/" + filename,
		"media_type": mediaType,
	})
}

// randomFileName draws 128 bits from crypto/rand, so knowing when a file was
// uploaded does not let anyone guess its URL.
func randomFileName(ext string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Practically unreachable, and better than a predictable name.
		log.Fatal("crypto/rand failed:", err)
	}
	return hex.EncodeToString(b) + ext
}

// sanitizeHeaderFilename keeps only characters that are safe in an HTTP
// header, replacing the rest with underscores.
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

func filesHandler(w http.ResponseWriter, r *http.Request) {
	// The cookie comes first because <img> and <video> send it
	// automatically; a bearer or query token is the fallback for API use.
	tok := ""
	if c, err := r.Cookie(authCookieName); err == nil {
		tok = c.Value
	}
	if tok == "" {
		tok = extractToken(r)
	}
	if _, ok := validateToken(tok); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	filename := filepath.Base(r.URL.Path)
	if filename == "." || filename == "/" || strings.Contains(filename, "..") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// private: attachments must not settle in shared caches or proxies.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// Images and video are inline so they display in the chat; everything
	// else downloads, since the browser must not render a document in this
	// origin's context.
	ext := strings.ToLower(filepath.Ext(filename))
	if mt, ok := allowedExts[ext]; !ok || (mt != "image" && mt != "video") {
		// The name comes from the URL and is therefore client-controlled, so
		// a quote in it would break out of the header's own quoting. Real
		// names are hex+ext, so anything outside that alphabet is stripped
		// rather than escaped. The original file name lives in the link's
		// fragment and never reaches the server.
		w.Header().Set("Content-Disposition", "attachment; filename=\""+sanitizeHeaderFilename(filename)+"\"")
	}

	// Decrypts and streams with Range support, or serves a legacy
	// unencrypted upload as plain static content.
	serveUploadedFile(w, r, filepath.Join(uploadDir(), filename), ext)
}
