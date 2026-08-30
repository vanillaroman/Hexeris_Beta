package main

// SERVER_ENC_KEY rotation (requires TEST_DATABASE_URL).
//
// Mainly one property is checked: a run interrupted halfway does not corrupt
// the data. Rotation is an operation that will be interrupted one day, and a
// second pass over an already re-encrypted row with the old key would turn it
// into rubbish invisibly, until the first read.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var rotSchemaOnce sync.Once

func setupRotate(t *testing.T) {
	t.Helper()
	setupIntegration(t)
	rotSchemaOnce.Do(func() {
		initEncKeyGuardSchema()
		initRotationSchema()
	})
	// uploadDir() is a sync.OnceValue and dies without the variable. The value
	// may have been frozen by an earlier test, so ours is substituted only when
	// there is none at all; the directory is created either way.
	if os.Getenv("UPLOAD_DIR") == "" {
		os.Setenv("UPLOAD_DIR", filepath.Join(os.TempDir(), "hexeris-rotate-test"))
	}
	// The directory is created from the variable and NOT from uploadDir():
	// uploadDir itself dies when the directory is missing, so it never gets there.
	if err := os.MkdirAll(os.Getenv("UPLOAD_DIR"), 0o700); err != nil {
		t.Fatalf("upload directory: %v", err)
	}
	// And this covers the case where the path was frozen by an earlier test on a
	// directory it has since cleaned up.
	if err := os.MkdirAll(uploadDir(), 0o700); err != nil {
		t.Fatalf("upload directory: %v", err)
	}
}

func keyOf(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// ── Message bodies ────────────────────────────────────────────────────────

func TestRotateBodyRoundTrip(t *testing.T) {
	oldK, newK := keyOf(0x11), keyOf(0x22)
	const plain = "quarterly report 🚀"

	stored, err := encryptBodyWith(oldK, plain)
	if err != nil {
		t.Fatal(err)
	}
	if stored == plain {
		t.Fatal("the body stayed in plaintext")
	}
	// A key that was not used for encryption must not "decrypt" — the whole
	// logic of telling the eras apart rests on that.
	if _, ok := decryptBodyWith(newK, stored); ok {
		t.Fatal("a foreign key worked — nothing distinguishes a re-encrypted row from an old one")
	}
	got, ok := decryptBodyWith(oldK, stored)
	if !ok || got != plain {
		t.Fatalf("the right key did not work: ok=%v got=%q", ok, got)
	}

	// And the other way round after re-encryption.
	re, _ := encryptBodyWith(newK, got)
	if _, ok := decryptBodyWith(oldK, re); ok {
		t.Fatal("the old key still works after re-encryption")
	}
	if v, ok := decryptBodyWith(newK, re); !ok || v != plain {
		t.Fatalf("the new key cannot read after re-encryption: ok=%v got=%q", ok, v)
	}
}

// Plaintext (a row from prehistoric times) must not count as "our" row under
// any key — otherwise rotation would "decrypt" it into rubbish.
func TestRotateLeavesPlaintextAlone(t *testing.T) {
	for _, s := range []string{"", "just text", "/files/photo.jpg", "not base64 at all!!"} {
		if _, ok := decryptBodyWith(keyOf(0x11), s); ok {
			t.Errorf("plaintext %q was taken for ciphertext", s)
		}
	}
}

// THE MAIN POINT FOR THE DATABASE: a second pass does not spoil what is done.
//
// That is what a restart after an interruption looks like: some rows on the
// new key, some on the old, and the run must sort itself out.
func TestRotateMessagesIsResumable(t *testing.T) {
	setupRotate(t)
	oldK, newK := keyOf(0x31), keyOf(0x32)
	a, b := uniqueName("rot_a"), uniqueName("rot_b")

	// One row "before the break" (old key), one "after" (already new).
	beforeID, afterID := uniqueName("m"), uniqueName("m")
	encOld, _ := encryptBodyWith(oldK, "not touched yet")
	encNew, _ := encryptBodyWith(newK, "already re-encrypted")
	for _, p := range []struct{ id, body string }{{beforeID, encOld}, {afterID, encNew}} {
		if _, err := db.Exec(
			`INSERT INTO messages(id, sender, recipient, body) VALUES($1,$2,$3,$4)`,
			p.id, a, b, p.body); err != nil {
			t.Fatalf("insert: %v", err)
		}
		id := p.id
		t.Cleanup(func() { db.Exec(`DELETE FROM messages WHERE id=$1`, id) })
	}

	st := &rotateStats{}
	rotateMessages(oldK, newK, false, st)

	read := func(id string) string {
		var body string
		if err := db.QueryRow(`SELECT body FROM messages WHERE id=$1`, id).Scan(&body); err != nil {
			t.Fatal(err)
		}
		v, ok := decryptBodyWith(newK, body)
		if !ok {
			t.Fatalf("row %s cannot be read with the new key", id)
		}
		return v
	}
	if got := read(beforeID); got != "not touched yet" {
		t.Errorf("the untouched row after rotation: %q", got)
	}
	if got := read(afterID); got != "already re-encrypted" {
		t.Fatalf("an ALREADY re-encrypted row was ruined by the second pass: %q", got)
	}

	// One more pass in a row must be harmless — that is exactly what a restart
	// after an interruption looks like.
	second := &rotateStats{}
	rotateMessages(oldK, newK, false, second)
	if got := read(afterID); got != "already re-encrypted" {
		t.Fatalf("the third pass ruined the row: %q", got)
	}

	// A repeat run must also RECOGNISE what is already done, not merely refrain
	// from spoiling it. Integrity here is held by GCM: the old key will not
	// verify on a re-encrypted row in any case. But without the "try the new key
	// first" check such a row would land in the counter of foreign and plaintext
	// rows, and the operator would see thousands of supposedly unreadable rows in
	// the report — that is, a reason to roll back a successful rotation.
	if second.msgRotated != 0 {
		t.Errorf("the repeat run re-encrypted something again: %d", second.msgRotated)
	}
	for _, id := range []string{beforeID, afterID} {
		var body string
		db.QueryRow(`SELECT body FROM messages WHERE id=$1`, id).Scan(&body)
		if _, ok := decryptBodyWith(newK, body); !ok {
			t.Fatalf("row %s is not on the new key", id)
		}
	}
	if second.msgAlready < 2 {
		t.Errorf("the repeat run recognised only %d rows of 2 as done — "+
			"the rest landed in the unreadable counter", second.msgAlready)
	}
}

// A dry run has no right to touch the data.
func TestRotateDryRunChangesNothing(t *testing.T) {
	setupRotate(t)
	oldK, newK := keyOf(0x41), keyOf(0x42)
	id := uniqueName("m")
	encOld, _ := encryptBodyWith(oldK, "do not touch")
	if _, err := db.Exec(`INSERT INTO messages(id, sender, recipient, body) VALUES($1,$2,$3,$4)`,
		id, uniqueName("rd_a"), uniqueName("rd_b"), encOld); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM messages WHERE id=$1`, id) })

	st := &rotateStats{}
	rotateMessages(oldK, newK, true, st)
	if st.msgRotated == 0 {
		t.Error("the dry run counted no work — it cannot be used to estimate the volume")
	}

	var body string
	db.QueryRow(`SELECT body FROM messages WHERE id=$1`, id).Scan(&body)
	if body != encOld {
		t.Fatal("the dry run changed the data")
	}
}

// ── Files ─────────────────────────────────────────────────────────────────

// writeEncFile writes a file in the [magic][IV][CTR] format under a given key.
func writeEncFile(t *testing.T, path string, key []byte, payload []byte) {
	t.Helper()
	iv := bytes.Repeat([]byte{0x07}, fileIVSize)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(payload))
	cipher.NewCTR(block, iv).XORKeyStream(ct, payload)
	out := append(append(append([]byte{}, fileMagic...), iv...), ct...)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readEncFile decrypts a file with a given key.
func readEncFile(t *testing.T, path string, key []byte) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < fileHeaderSize {
		t.Fatalf("the file is shorter than the header: %d bytes", len(raw))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := raw[fileHeaderSize:]
	out := make([]byte, len(ct))
	cipher.NewCTR(block, raw[len(fileMagic):fileHeaderSize]).XORKeyStream(out, ct)
	return out
}

func TestRotateOneFileReEncrypts(t *testing.T) {
	oldK, newK := keyOf(0x51), keyOf(0x52)
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.jpg")
	tmp := path + ".rotating"
	payload := bytes.Repeat([]byte("media payload "), 500) // certainly larger than a block

	writeEncFile(t, path, oldK, payload)
	if err := rotateOneFile(path, tmp, oldK, newK); err != nil {
		t.Fatalf("re-encryption: %v", err)
	}

	// The original is untouched — only the caller's rename replaces it.
	if got := readEncFile(t, path, oldK); !bytes.Equal(got, payload) {
		t.Fatal("the original was modified before the rename")
	}
	if got := readEncFile(t, tmp, newK); !bytes.Equal(got, payload) {
		t.Fatal("the content after re-encryption does not match the original")
	}
	// A new IV, not a reused one: the (key, IV) pair must never repeat in CTR.
	oldHead, _ := os.ReadFile(path)
	newHead, _ := os.ReadFile(tmp)
	if bytes.Equal(oldHead[len(fileMagic):fileHeaderSize], newHead[len(fileMagic):fileHeaderSize]) {
		t.Error("the IV was reused")
	}
}

func TestFileIsEncryptedDetectsLegacy(t *testing.T) {
	dir := t.TempDir()
	enc := filepath.Join(dir, "enc.bin")
	writeEncFile(t, enc, keyOf(0x61), []byte("payload"))
	plain := filepath.Join(dir, "plain.txt")
	os.WriteFile(plain, []byte("uploaded before encryption was enabled"), 0o600)
	tiny := filepath.Join(dir, "tiny.bin")
	os.WriteFile(tiny, []byte("ab"), 0o600)

	for _, c := range []struct {
		path string
		want bool
	}{{enc, true}, {plain, false}, {tiny, false}} {
		got, err := fileIsEncrypted(c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		if got != c.want {
			t.Errorf("%s: detected as encrypted=%v, expected %v", filepath.Base(c.path), got, c.want)
		}
	}
}

// THE MAIN POINT FOR FILES: a break between the journal entry and the rename.
//
// At that moment the original is still on the old key while a finished
// .rotating sits next to it. The run must finish the move rather than walk
// past (the file would stay unreadable) or redo it from the old key.
func TestRotateFilesFinishesInterruptedRename(t *testing.T) {
	setupRotate(t)
	oldK, newK := keyOf(0x71), keyOf(0x72)
	newFP := encKeyFingerprint(newK)

	dir := uploadDir()
	name := uniqueName("rotf") + ".bin"
	path := filepath.Join(dir, name)
	tmp := path + ".rotating"
	payload := []byte("content that must survive an interruption")

	writeEncFile(t, path, oldK, payload) // the original: the old key
	writeEncFile(t, tmp, newK, payload)  // .rotating: already the new one
	if _, err := db.Exec(`INSERT INTO enc_rotation(kind,item,key_fp) VALUES('file',$1,$2)`,
		name, newFP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Remove(path)
		os.Remove(tmp)
		db.Exec(`DELETE FROM enc_rotation WHERE item=$1`, name)
	})

	st := &rotateStats{}
	rotateFiles(oldK, newK, false, st)

	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("the temporary file was not removed — the move is unfinished")
	}
	if got := readEncFile(t, path, newK); !bytes.Equal(got, payload) {
		t.Fatal("the file cannot be read with the new key after the move was finished")
	}
	if st.fileFailed != 0 {
		t.Errorf("errors: %d", st.fileFailed)
	}
}

// A break BEFORE the entry: .rotating is unfinished rubbish, the original is
// intact. The run must throw the rubbish away and redo the work.
func TestRotateFilesDiscardsUnjournaledTemp(t *testing.T) {
	setupRotate(t)
	oldK, newK := keyOf(0x81), keyOf(0x82)

	dir := uploadDir()
	name := uniqueName("rotg") + ".bin"
	path := filepath.Join(dir, name)
	tmp := path + ".rotating"
	payload := []byte("the original is intact")

	writeEncFile(t, path, oldK, payload)
	os.WriteFile(tmp, []byte("unfinished rubbish"), 0o600) // no journal entry
	t.Cleanup(func() {
		os.Remove(path)
		os.Remove(tmp)
		db.Exec(`DELETE FROM enc_rotation WHERE item=$1`, name)
	})

	st := &rotateStats{}
	rotateFiles(oldK, newK, false, st)

	if st.fileFailed != 0 {
		t.Fatalf("errors: %d", st.fileFailed)
	}
	if got := readEncFile(t, path, newK); !bytes.Equal(got, payload) {
		t.Fatalf("the file is ruined: %q", got)
	}
	// The entry now exists — a repeat run will not touch the file again.
	if !rotationDone("file", name, encKeyFingerprint(newK)) {
		t.Error("the file was re-encrypted but not journalled — a second pass would kill it")
	}
	before := readEncFile(t, path, newK)
	rotateFiles(oldK, newK, false, &rotateStats{})
	if got := readEncFile(t, path, newK); !bytes.Equal(got, before) {
		t.Fatal("the repeat run ruined an already re-encrypted file")
	}
}
