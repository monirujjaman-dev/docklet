package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docklet.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	n, err := s.Q.CountUsers(ctx)
	if err != nil || n != 0 {
		t.Fatalf("CountUsers = %d, %v", n, err)
	}
	var mode string
	if err := s.DB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("db mode = %v, want 0600", st.Mode().Perm())
	}
	_ = s.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if err := s.Q.InsertAuditEntry(ctx, db.InsertAuditEntryParams{
		ID: 1, Ts: 1, Actor: "system", Action: "test", Details: "{}", PrevHash: "x", Hash: "y",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "UPDATE audit_log SET action = 'evil'"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE not blocked: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, "DELETE FROM audit_log"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE not blocked: %v", err)
	}
}

func TestTxRollsBack(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	errBoom := context.Canceled
	err := s.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.CreateUser(ctx, db.CreateUserParams{Username: "a", PasswordHash: "h", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := s.Q.CountUsers(ctx); n != 0 {
		t.Fatalf("user count = %d after rollback", n)
	}
}

func TestUsernameCaseInsensitiveUnique(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.Q.CreateUser(ctx, db.CreateUserParams{Username: "Admin", PasswordHash: "h", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Q.CreateUser(ctx, db.CreateUserParams{Username: "admin", PasswordHash: "h", CreatedAt: 1, UpdatedAt: 1}); err == nil {
		t.Fatal("duplicate username (different case) accepted")
	}
	u, err := s.Q.GetUserByUsername(ctx, "ADMIN")
	if err != nil || u.Username != "Admin" {
		t.Fatalf("lookup: %+v %v", u, err)
	}
}
