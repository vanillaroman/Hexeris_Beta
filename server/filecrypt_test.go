package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"io"
	"testing"
)

// decryptRange decrypts [start,end] out of an encryptStream buffer, mirroring
// serveUploadedFile without the HTTP layer.
func decryptRange(t *testing.T, enc []byte, start, length int64) []byte {
	t.Helper()
	iv := enc[4:fileHeaderSize]
	stream, err := newCTRAt(iv, start)
	if err != nil {
		t.Fatalf("newCTRAt: %v", err)
	}
	r := io.LimitReader(bytes.NewReader(enc[int64(fileHeaderSize)+start:]), length)
	sr := &cipher.StreamReader{S: stream, R: r}
	out, err := io.ReadAll(sr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

func TestEncryptStreamRoundTrip(t *testing.T) {
	encKey = func() []byte { return bytes.Repeat([]byte{0x42}, 32) } // test key

	// Larger than a few AES blocks, to catch counter arithmetic errors.
	plain := make([]byte, aes.BlockSize*40+7)
	for i := range plain {
		plain[i] = byte(i * 31)
	}

	var enc bytes.Buffer
	if err := encryptStream(&enc, bytes.NewReader(plain)); err != nil {
		t.Fatalf("encryptStream: %v", err)
	}
	if !bytes.HasPrefix(enc.Bytes(), fileMagic) {
		t.Fatal("missing magic prefix")
	}
	if enc.Len() != len(plain)+fileHeaderSize {
		t.Fatalf("size: got %d want %d", enc.Len(), len(plain)+fileHeaderSize)
	}

	// Full decryption.
	full := decryptRange(t, enc.Bytes(), 0, int64(len(plain)))
	if !bytes.Equal(full, plain) {
		t.Fatal("full decrypt mismatch")
	}

	// Ranges with different alignments against the 16-byte block.
	offsets := []int64{1, 15, 16, 17, aes.BlockSize * 3, aes.BlockSize*5 + 9, int64(len(plain)) - 10}
	for _, off := range offsets {
		length := int64(len(plain)) - off
		if length > 33 {
			length = 33
		}
		got := decryptRange(t, enc.Bytes(), off, length)
		want := plain[off : off+length]
		if !bytes.Equal(got, want) {
			t.Fatalf("range off=%d len=%d mismatch\n got %x\nwant %x", off, length, got, want)
		}
	}
}

func TestParseSingleRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		hdr                string
		wantOK             bool
		wantStart, wantEnd int64
	}{
		{"bytes=0-99", true, 0, 99},
		{"bytes=100-", true, 100, 999},
		{"bytes=-50", true, 950, 999},
		{"bytes=500-1000000", true, 500, 999}, // end is clamped to size-1
		{"bytes=0-99,200-299", false, 0, 0},   // multi-range is ignored
		{"items=0-99", false, 0, 0},           // not a bytes unit
		{"bytes=1000-1001", false, 0, 0},      // start beyond the end
		{"bytes=abc-def", false, 0, 0},
	}
	for _, c := range cases {
		s, e, ok := parseSingleRange(c.hdr, size)
		if ok != c.wantOK || (ok && (s != c.wantStart || e != c.wantEnd)) {
			t.Errorf("%q: got (%d,%d,%v) want (%d,%d,%v)", c.hdr, s, e, ok, c.wantStart, c.wantEnd, c.wantOK)
		}
	}
}
