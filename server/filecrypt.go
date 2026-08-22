package main

// At-rest encryption for uploaded files.
//
// Message bodies use AES-256-GCM (encryptBody in main.go). Files are a
// different problem: they are large and must stream with Range support,
// because seeking in video requires 206 responses. AES-256-CTR decrypts any
// byte offset independently, so a range can be served without reading the
// whole file.
//
// What this does and does not give:
//   - Confidentiality on disk and in backups: a stolen volume or dump is
//     useless without SERVER_ENC_KEY. That is the goal.
//   - Not end-to-end: the server holds the key and can read the content.
//   - Unlike GCM, CTR has no integrity tag. The trade is deliberate, for
//     seekable streaming: an attacker able to rewrite files on the server's
//     own disk already has write access, which is a worse problem than
//     tampered media.
//
// On-disk layout: [4-byte magic "HXE1"][16-byte IV][ciphertext]. The magic
// lets the file handler tell encrypted files from legacy ones uploaded before
// encryption was enabled and serve those unchanged, so no migration is needed.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
)

var fileMagic = []byte("HXE1")

const (
	fileIVSize     = 16
	fileHeaderSize = 4 + fileIVSize // magic + IV
)

// encryptStream writes the header and then encrypts src into dst with
// AES-256-CTR. Memory use does not grow with file size.
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

// newCTRAt returns a CTR stream positioned at a plaintext offset. CTR treats
// the IV as a 128-bit big-endian counter incremented per 16-byte block, so
// reaching an arbitrary byte means adding the block number to the counter and
// burning the remainder inside that block.
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
		stream.XORKeyStream(skip, skip) // advance the keystream to the byte
	}
	return stream, nil
}

// addToCounter adds n to the 128-bit big-endian counter, carrying between
// the high and low 64-bit words.
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

// serveUploadedFile streams a file, decrypting it with support for a single
// Range when the magic matches, and falling back to http.ServeFile for legacy
// plaintext uploads.
func serveUploadedFile(w http.ResponseWriter, r *http.Request, path, ext string) {
	f, err := os.Open(path) // #nosec G304 — path is filepath.Base under uploadDir
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	header := make([]byte, fileHeaderSize)
	n, _ := io.ReadFull(f, header)
	if n < fileHeaderSize || !bytes.HasPrefix(header, fileMagic) {
		// Legacy unencrypted upload: serve it as a plain static file.
		http.ServeFile(w, r, path)
		return
	}
	iv := header[4:fileHeaderSize]

	info, err := f.Stat()
	if err != nil {
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
		// An unparsable or multi-part range falls back to the whole file:
		// a server may ignore Range, and browsers cope with that.
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
	_, _ = io.Copy(w, sr) // the client may hang up; a copy error is not ours
}

// contentTypeFor maps an extension to a MIME type, defaulting to
// application/octet-stream, which is safe together with nosniff.
func contentTypeFor(ext string) string {
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// parseSingleRange parses one HTTP range: "bytes=start-end", "bytes=start-"
// or "bytes=-suffix". ok=false means the range should be ignored and the
// whole file served.
func parseSingleRange(header string, size int64) (start, end int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(header, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, prefix)
	if strings.Contains(spec, ",") {
		return 0, 0, false // multi-range requests are not supported
	}
	i := strings.IndexByte(spec, '-')
	if i < 0 {
		return 0, 0, false
	}
	startStr := strings.TrimSpace(spec[:i])
	endStr := strings.TrimSpace(spec[i+1:])

	if startStr == "" { // suffix range: the last N bytes
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
