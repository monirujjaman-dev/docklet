// Package auth holds credential primitives: argon2id password hashing,
// TOTP (RFC 6238) and session tokens.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. OWASP's minimum recommendation (19 MiB, t=2, p=1)
// keeps a login well under 100 ms and the memory spike small on a 1 GB VPS.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// MinPasswordLength is the minimum accepted password length.
const MinPasswordLength = 10

// MaxPasswordLength bounds hashing cost for hostile input.
const MaxPasswordLength = 256

// ErrInvalidHash is returned for malformed stored hashes.
var ErrInvalidHash = errors.New("auth: invalid password hash")

// dummyHash is verified against when a username does not exist, so the
// response time does not reveal whether an account exists.
var dummyHash = mustHash("docklet-dummy-password")

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	switch {
	case len(pw) < MinPasswordLength:
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	case len(pw) > MaxPasswordLength:
		return fmt.Errorf("password must be at most %d characters", MaxPasswordLength)
	}
	return nil
}

// Each argon2id call allocates ~19 MiB. Go keeps freed heap around for reuse,
// which would leave the idle control plane well above its RSS budget after a
// login. Logins are rare, so we hand the memory back to the OS shortly after
// the last hash, coalescing bursts into a single release.
var releasePending atomic.Bool

func releaseMemory() {
	if releasePending.Swap(true) {
		return
	}
	time.AfterFunc(2*time.Second, func() {
		releasePending.Store(false)
		debug.FreeOSMemory()
	})
}

// HashPassword returns a PHC-formatted argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	releaseMemory()
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a PHC argon2id hash in constant time.
func VerifyPassword(pw, encoded string) (bool, error) {
	if len(pw) > MaxPasswordLength {
		return false, nil
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrInvalidHash
	}
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false, ErrInvalidHash
	}
	if mem == 0 || mem > 1024*1024 || t == 0 || t > 16 || p == 0 {
		return false, ErrInvalidHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 128 {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(pw), salt, t, mem, p, uint32(len(want))) // #nosec G115 -- len bounded above
	releaseMemory()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// BurnVerify spends the same time as a real verification. Call it when the
// user does not exist.
func BurnVerify(pw string) {
	_, _ = VerifyPassword(pw, dummyHash)
}

func mustHash(pw string) string {
	h, err := HashPassword(pw)
	if err != nil {
		panic(err)
	}
	return h
}
