package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected format %q", h)
	}
	ok, err := VerifyPassword("correct horse battery", h)
	if err != nil || !ok {
		t.Fatalf("valid password rejected: ok=%v err=%v", ok, err)
	}
	ok, _ = VerifyPassword("wrong password!!", h)
	if ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salt not random")
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=99999999,t=1,p=1$AA$AA"} {
		if _, err := VerifyPassword("x", h); err == nil {
			t.Errorf("expected error for %q", h)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if ValidatePassword("short") == nil {
		t.Error("short password accepted")
	}
	if ValidatePassword(strings.Repeat("a", MaxPasswordLength+1)) == nil {
		t.Error("overlong password accepted")
	}
	if err := ValidatePassword("long enough pw"); err != nil {
		t.Error(err)
	}
}

// RFC 6238 appendix B test vectors (SHA1, secret "12345678901234567890"),
// truncated to 6 digits.
func TestTOTPRFCVectors(t *testing.T) {
	key := []byte("12345678901234567890")
	cases := map[int64]string{
		59:          "287082",
		1111111109:  "081804",
		1111111111:  "050471",
		1234567890:  "005924",
		2000000000:  "279037",
		20000000000: "353130",
	}
	for ts, want := range cases {
		if got := hotp(key, ts/30, 6); got != want {
			t.Errorf("t=%d: got %s, want %s", ts, got, want)
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := b32.DecodeString(secret)
	now := time.Unix(1_800_000_000, 0)
	step := now.Unix() / 30

	code := hotp(key, step, 6)
	got, ok := VerifyTOTP(secret, code, now, 0)
	if !ok || got != step {
		t.Fatalf("current code rejected: ok=%v step=%d", ok, got)
	}
	if _, ok := VerifyTOTP(secret, code, now, step); ok {
		t.Fatal("replayed code accepted")
	}
	if _, ok := VerifyTOTP(secret, hotp(key, step-1, 6), now, 0); !ok {
		t.Fatal("previous-step code rejected (skew)")
	}
	if _, ok := VerifyTOTP(secret, hotp(key, step-3, 6), now, 0); ok {
		t.Fatal("old code accepted")
	}
	if _, ok := VerifyTOTP(secret, "12345", now, 0); ok {
		t.Fatal("short code accepted")
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("Docklet", "admin", "ABC")
	if !strings.HasPrefix(u, "otpauth://totp/Docklet:admin?") || !strings.Contains(u, "secret=ABC") {
		t.Fatalf("bad uri %q", u)
	}
}

func TestTokenHashing(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if a == b || len(a) < 40 {
		t.Fatalf("weak tokens %q %q", a, b)
	}
	if HashToken(a) == a || len(HashToken(a)) != 64 {
		t.Fatal("bad hash")
	}
}
