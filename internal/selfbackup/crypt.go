package selfbackup

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// Encrypted archives are a header followed by length-prefixed chunks, each
// sealed with XChaCha20-Poly1305 under a key derived from the passphrase with
// argon2id. A chunk's nonce is the random prefix plus its index, and the
// header and a "last chunk" flag are authenticated, so chunks can't be
// reordered, dropped or truncated without detection.
//
//	magic(8) | salt(16) | time(4) | memoryKiB(4) | threads(1) | noncePrefix(16)
//	{ length(4) | ciphertext(length) }*

var magic = []byte("DKLTBK01")

const (
	chunkSize   = 64 << 10
	headerSize  = 8 + 16 + 4 + 4 + 1 + 16
	argonTime   = 3
	argonMemKiB = 64 << 10
	argonThread = 2
	maxMemKiB   = 1 << 20 // refuse absurd parameters from a crafted header
)

// ErrPassphrase means the passphrase is wrong or the archive was altered.
var ErrPassphrase = errors.New("wrong passphrase, or the backup file is corrupted")

// ErrNeedPassphrase means the archive is encrypted and no passphrase was given.
var ErrNeedPassphrase = errors.New("this backup is encrypted: a passphrase is required")

func deriveKey(pass string, salt []byte, t, m uint32, p uint8) []byte {
	return argon2.IDKey([]byte(pass), salt, t, m, p, chacha20poly1305.KeySize)
}

type sealWriter struct {
	w      io.Writer
	header []byte
	prefix []byte
	aead   interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
	}
	buf    []byte
	n      uint64
	closed bool
}

func newSealWriter(w io.Writer, pass string) (*sealWriter, error) {
	h := make([]byte, headerSize)
	copy(h, magic)
	salt, prefix := h[8:24], h[33:49]
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint32(h[24:28], argonTime)
	binary.BigEndian.PutUint32(h[28:32], argonMemKiB)
	h[32] = argonThread
	aead, err := chacha20poly1305.NewX(deriveKey(pass, salt, argonTime, argonMemKiB, argonThread))
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(h); err != nil {
		return nil, err
	}
	return &sealWriter{w: w, header: h, prefix: prefix, aead: aead, buf: make([]byte, 0, chunkSize)}, nil
}

func chunkNonce(prefix []byte, n uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[16:], n)
	return nonce
}

func chunkAAD(header []byte, last bool) []byte {
	aad := append([]byte{}, header...)
	if last {
		return append(aad, 1)
	}
	return append(aad, 0)
}

func (s *sealWriter) flush(last bool) error {
	ct := s.aead.Seal(nil, chunkNonce(s.prefix, s.n), s.buf, chunkAAD(s.header, last))
	s.n++
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct))) //nolint:gosec // at most chunkSize+16
	if _, err := s.w.Write(l[:]); err != nil {
		return err
	}
	_, err := s.w.Write(ct)
	s.buf = s.buf[:0]
	return err
}

func (s *sealWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := copy(s.buf[len(s.buf):chunkSize], p)
		s.buf = s.buf[:len(s.buf)+n]
		p = p[n:]
		if len(s.buf) == chunkSize {
			if err := s.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

// Close seals the final chunk (possibly empty).
func (s *sealWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

type openReader struct {
	r      *bufio.Reader
	header []byte
	prefix []byte
	aead   interface {
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	}
	buf  []byte
	n    uint64
	done bool
}

func newOpenReader(r *bufio.Reader, pass string) (*openReader, error) {
	h := make([]byte, headerSize)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if !bytes.Equal(h[:8], magic) {
		return nil, errors.New("not a Docklet backup")
	}
	t, m, p := binary.BigEndian.Uint32(h[24:28]), binary.BigEndian.Uint32(h[28:32]), h[32]
	if t == 0 || t > 20 || m == 0 || m > maxMemKiB || p == 0 {
		return nil, errors.New("unsupported key-derivation parameters in the backup header")
	}
	aead, err := chacha20poly1305.NewX(deriveKey(pass, h[8:24], t, m, p))
	if err != nil {
		return nil, err
	}
	return &openReader{r: r, header: h, prefix: h[33:49], aead: aead}, nil
}

func (o *openReader) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.done {
			return 0, io.EOF
		}
		var l [4]byte
		if _, err := io.ReadFull(o.r, l[:]); err != nil {
			return 0, fmt.Errorf("backup is truncated: %w", err)
		}
		size := binary.BigEndian.Uint32(l[:])
		if size > chunkSize+chacha20poly1305.Overhead {
			return 0, ErrPassphrase
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(o.r, ct); err != nil {
			return 0, fmt.Errorf("backup is truncated: %w", err)
		}
		nonce := chunkNonce(o.prefix, o.n)
		pt, err := o.aead.Open(nil, nonce, ct, chunkAAD(o.header, false))
		if err != nil {
			if pt, err = o.aead.Open(nil, nonce, ct, chunkAAD(o.header, true)); err != nil {
				return 0, ErrPassphrase
			}
			o.done = true
			if _, err := o.r.Peek(1); err != io.EOF {
				return 0, errors.New("unexpected data after the end of the backup")
			}
		}
		o.n++
		o.buf = pt
	}
	n := copy(p, o.buf)
	o.buf = o.buf[n:]
	return n, nil
}
