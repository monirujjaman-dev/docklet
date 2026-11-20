// Package secrets encrypts sensitive values at rest with XChaCha20-Poly1305
// using a 32-byte master key stored in a separate file.
package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

// KeySize is the master key length in bytes.
const KeySize = chacha20poly1305.KeySize

// ErrDecrypt is returned when a ciphertext cannot be authenticated.
var ErrDecrypt = errors.New("secrets: decryption failed")

// Box encrypts and decrypts values with the master key.
type Box struct {
	key []byte
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: key must be %d bytes, got %d", KeySize, len(key))
	}
	k := make([]byte, KeySize)
	copy(k, key)
	return &Box{key: k}, nil
}

// LoadOrCreateKey reads the master key at path, creating it (mode 0600) if it
// does not exist. An existing key file with group/other permissions is rejected.
func LoadOrCreateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path) // #nosec G304 -- path comes from operator config
	if err == nil {
		st, statErr := os.Stat(path)
		if statErr != nil {
			return nil, statErr
		}
		if st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("secrets: %s must not be accessible by group/other (mode %v); run: chmod 600 %s", path, st.Mode().Perm(), path)
		}
		if len(key) != KeySize {
			return nil, fmt.Errorf("secrets: %s has %d bytes, want %d", path, len(key), KeySize)
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	key = make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

// Seal encrypts plaintext. aad binds the ciphertext to its context (for
// example "user:42:totp") so it cannot be swapped into another row.
// Output layout: nonce (24 bytes) || ciphertext+tag.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(b.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(b.key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, ct := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
