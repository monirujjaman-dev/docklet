package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpenRoundTrip(t *testing.T) {
	b := testBox(t)
	ct, err := b.Seal([]byte("hunter2"), []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("hunter2")) {
		t.Fatal("ciphertext contains plaintext")
	}
	pt, err := b.Open(ct, []byte("ctx"))
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "hunter2" {
		t.Fatalf("got %q", pt)
	}
}

func TestOpenRejectsWrongAADAndTamper(t *testing.T) {
	b := testBox(t)
	ct, _ := b.Seal([]byte("v"), []byte("a"))
	if _, err := b.Open(ct, []byte("b")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong aad: err = %v", err)
	}
	ct[len(ct)-1] ^= 1
	if _, err := b.Open(ct, []byte("a")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered: err = %v", err)
	}
	if _, err := b.Open([]byte("short"), nil); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("short: err = %v", err)
	}
}

func TestNonceIsRandom(t *testing.T) {
	b := testBox(t)
	a, _ := b.Seal([]byte("same"), nil)
	c, _ := b.Seal([]byte("same"), nil)
	if bytes.Equal(a, c) {
		t.Fatal("two seals of the same plaintext are identical")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "master.key")
	k1, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("key changed on reload")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("expected error for world-readable key")
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("expected error")
	}
}
