package main

// Encryption of uploaded files on disk (at rest).
//
// Message bodies are encrypted with AES-256-GCM (see encryptBody in main.go).
// Files are a different story: they are large (up to 60 MB) and must be served
// as a stream with Range support (seeking in a video on Safari/iOS requires a
// 206). AES-256-CTR is therefore used here: any byte offset decrypts
// independently, so Range requests work without reading the whole file.
//
// What this gives, and what it does NOT:
//   • Confidentiality on disk and in backups — a stolen volume or dump is
//     useless without SERVER_ENC_KEY. That is the point.
//   • NOT end-to-end: the server holds the key and sees the content, exactly as
//     it does with message bodies.
//   • Unlike GCM, CTR carries no integrity tag. The trade-off is deliberate,
//     for the sake of streaming and seeking. The threat "an active attacker
//     rewrote our file on our own disk" already implies write access to the
//     server — where there are worse problems than substituted media.
//
// The on-disk format: [4 bytes of magic "HXE1"][16 bytes of IV][ciphertext].
// The magic lets filesHandler tell encrypted files from "legacy" ones, uploaded
// before encryption was switched on, and serve the old ones as they are — no
// migration is needed and nothing breaks.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var fileMagic = []byte("HXE1")

const (
	fileIVSize     = 16
	fileHeaderSize = 4 + fileIVSize // magic + IV
)

// encryptStream writes the header ([magic][random IV]) into dst and then
// encrypts src as a stream in AES-256-CTR mode. Memory does not grow with the
// file size — io.Copy works in chunks.
func encryptStream(dst io.Writer, src io.Reader) error {
	iv := make([]byte, fileIVSize)
	if _, err := rand.Read(iv); err != nil {
		return err
	}
	if _, err := dst.Write(fileMagic); err != nil {
		return err
	}
	if _, err := dst.Write(iv); err != nil {
		return err
	}
	block, err := aes.NewCipher(encKey())
	if err != nil {
		return err
	}
	sw := &cipher.StreamWriter{S: cipher.NewCTR(block, iv), W: dst}
	_, err = io.Copy(sw, src)
	return err
}

// newCTRAt returns a CTR stream positioned exactly at the plaintext offset.
// CTR treats the IV as a 128-bit big-endian counter incremented for every
// 16-byte block; to land on an arbitrary byte, the block number is added to the
// counter and the remainder inside the block is burned off.
func newCTRAt(iv []byte, offset int64) (cipher.Stream, error) {
	block, err := aes.NewCipher(encKey())
	if err != nil {
		return nil, err
	}
	ctr := make([]byte, fileIVSize)
	copy(ctr, iv)
	addToCounter(ctr, uint64(offset/aes.BlockSize))
	stream := cipher.NewCTR(block, ctr)
	if within := int(offset % aes.BlockSize); within > 0 {
		skip := make([]byte, within)
		stream.XORKeyStream(skip, skip) // advance the keystream to the byte we want
	}
	return stream, nil
}

// addToCounter adds n to the 128-bit big-endian counter ctr (16 bytes),
// carrying between the high and low 64-bit words.
func addToCounter(ctr []byte, n uint64) {
	hi := binary.BigEndian.Uint64(ctr[0:8])
	lo := binary.BigEndian.Uint64(ctr[8:16])
	newLo := lo + n
	if newLo < lo { // carry into the high word
		hi++
	}
	binary.BigEndian.PutUint64(ctr[0:8], hi)
	binary.BigEndian.PutUint64(ctr[8:16], newLo)
}

// describeDirFor reports what the process sees in the file's directory.
//
// For the log only, and only on failure: it is not free (it reads the
// directory), but it happens only when a file is missing, and the cost of that
// error is hours of blind investigation.
func describeDirFor(path string) string {
	dir := filepath.Dir(path)
	f, err := os.Open(dir) // #nosec G304 — our own directory, from uploadDir
	if err != nil {
		return fmt.Sprintf("the process cannot access directory %q: %v", dir, err)
	}
	defer f.Close()
	// Read a bounded amount: the upload directory may hold tens of thousands of
	// files, and dumping them into the log is not acceptable.
	names, err := f.Readdirnames(20)
	if err != nil && err != io.EOF {
		return fmt.Sprintf("directory %q cannot be read: %v", dir, err)
	}
	if len(names) == 0 {
		return fmt.Sprintf("the process sees directory %q as EMPTY — almost certainly "+
			"the service has its own namespace (ProtectHome/PrivateTmp/RootDirectory in the unit file)", dir)
	}
	return fmt.Sprintf("the process does see directory %q; it contains, for example: %v", dir, names[:min2(len(names), 5)])
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// serveUploadedFile serves the file at path. If the file is encrypted (the
// magic matched) it is decrypted as a stream with support for a single Range;
// otherwise (legacy plaintext) it goes out through http.ServeFile as before.
func serveUploadedFile(w http.ResponseWriter, r *http.Request, path, ext string) {
	f, err := os.Open(path) // #nosec G304 — path is built from filepath.Base + uploadDir
	if err != nil {
		// TELL APART "no such file" and "could not open it". Any error used to
		// produce a 404 here, so a permission refusal or a wrong UPLOAD_DIR
		// looked like a missing file: the administrator sees "404" in the
		// browser, goes to the directory, finds the file there — and the
		// investigation hits a dead end. The real cause reached nowhere.
		if os.IsNotExist(err) {
			// This case is logged TOO — it is the most common one, and without a
			// record the investigation runs into nothing: the browser shows a
			// 404, the log is silent, and it is not even clear whether the
			// request reached the application. The full path is the key part
			// here: it shows WHAT the server tried to open and whether that
			//
			// matches where the file actually is. Alongside it, what the process
			// SEES in the directory. Without that there is an unresolvable
			// argument: the administrator shows the file with ls while the
			// application answers "no such thing". The discrepancy means not a
			// missing file but different visibility — usually a systemd sandbox
			// (ProtectHome, PrivateTmp, RootDirectory, BindPaths) or a mount.
			log.Printf("files: no file at path %q; %s", path, describeDirFor(path))
			noStore(w)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// The path and the system cause go to the log; the outside gets a
		// generic text so the filesystem layout is not revealed.
		log.Printf("files: could not open %q: %v", path, err)
		noStore(w)
		http.Error(w, "the file cannot be read on the server", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	header := make([]byte, fileHeaderSize)
	n, _ := io.ReadFull(f, header)
	if n < fileHeaderSize || !hasPrefix(header, fileMagic) {
		// A legacy file with no encryption — serve it as ordinary static content.
		http.ServeFile(w, r, path)
		return
	}
	iv := header[4:fileHeaderSize]

	info, err := f.Stat()
	if err != nil {
		noStore(w)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	plainSize := info.Size() - fileHeaderSize

	if ct := contentTypeFor(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")

	start, end := int64(0), plainSize-1
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" && plainSize > 0 {
		if s, e, ok := parseSingleRange(rng, plainSize); ok {
			start, end = s, e
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, plainSize))
		}
		// An unparseable or multi-part range: serve the whole file (200)
		// silently. A server may ignore Range, and browsers cope with that.
	}
	if plainSize < 0 {
		plainSize = 0
		start, end = 0, -1
	}
	length := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))

	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}

	stream, err := newCTRAt(iv, start)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if _, err := f.Seek(fileHeaderSize+start, io.SeekStart); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	sr := &cipher.StreamReader{S: stream, R: io.LimitReader(f, length)}
	_, _ = io.Copy(w, sr) // the connection may have been dropped — ignore the copy error
}

// contentTypeFor returns the MIME type for an extension; unknown ones get
// application/octet-stream, which is safe together with nosniff.
func contentTypeFor(ext string) string {
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// parseSingleRange parses a single HTTP Range of the form "bytes=start-end",
// "bytes=start-" or "bytes=-suffix". ok=false means the range should be ignored
// and the whole file served.
func parseSingleRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, prefix)
	if strings.Contains(spec, ",") {
		return 0, 0, false // multiple ranges are not supported
	}
	i := strings.IndexByte(spec, '-')
	if i < 0 {
		return 0, 0, false
	}
	startStr := strings.TrimSpace(spec[:i])
	endStr := strings.TrimSpace(spec[i+1:])

	if startStr == "" { // a suffix range: the last N bytes
		nn, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || nn <= 0 {
			return 0, 0, false
		}
		if nn > size {
			nn = size
		}
		return size - nn, size - 1, true
	}

	s, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || s < 0 || s >= size {
		return 0, 0, false
	}
	e := size - 1
	if endStr != "" {
		e, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil || e < s {
			return 0, 0, false
		}
		if e >= size {
			e = size - 1
		}
	}
	return s, e, true
}

// hasPrefix — bytes.HasPrefix without importing bytes into this file.
func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}
