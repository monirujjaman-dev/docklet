package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendN(t *testing.T, s *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	uid := int64(1)
	for i := 0; i < n; i++ {
		if _, err := Append(ctx, s.Q, Entry{
			ActorID: &uid, Actor: "admin", Action: ActionLoginSucceeded, IP: "127.0.0.1",
			Details: map[string]any{"i": i},
		}, time.Unix(1_800_000_000+int64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChainVerifies(t *testing.T) {
	s := openStore(t)
	appendN(t, s, 25)
	res, err := Verify(context.Background(), s.Q)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Checked != 25 {
		t.Fatalf("res = %+v", res)
	}
}

func TestEmptyLogVerifies(t *testing.T) {
	s := openStore(t)
	res, err := Verify(context.Background(), s.Q)
	if err != nil || !res.OK || res.Checked != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

// Simulates an attacker with file access who drops the triggers first.
func tamper(t *testing.T, s *store.Store, sql string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		"DROP TRIGGER audit_log_no_update",
		"DROP TRIGGER audit_log_no_delete",
		sql,
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestDetectsEditedEntry(t *testing.T) {
	s := openStore(t)
	appendN(t, s, 10)
	tamper(t, s, "UPDATE audit_log SET actor = 'someone-else' WHERE id = 4")
	res, _ := Verify(context.Background(), s.Q)
	if res.OK || res.BrokenAt != 4 {
		t.Fatalf("res = %+v, want broken at 4", res)
	}
}

func TestDetectsDeletedEntry(t *testing.T) {
	s := openStore(t)
	appendN(t, s, 10)
	tamper(t, s, "DELETE FROM audit_log WHERE id = 6")
	res, _ := Verify(context.Background(), s.Q)
	if res.OK || res.BrokenAt != 7 {
		t.Fatalf("res = %+v, want broken at 7", res)
	}
}

func TestDetectsRehashedEntry(t *testing.T) {
	// Even recomputing one row's hash breaks the next row's prev_hash link.
	s := openStore(t)
	appendN(t, s, 5)
	tamper(t, s, "UPDATE audit_log SET hash = 'f00' WHERE id = 2")
	res, _ := Verify(context.Background(), s.Q)
	if res.OK || res.BrokenAt != 2 {
		t.Fatalf("res = %+v", res)
	}
}

func TestAppendRequiresActorAndAction(t *testing.T) {
	s := openStore(t)
	if _, err := Append(context.Background(), s.Q, Entry{Action: "x"}, time.Now()); err == nil {
		t.Fatal("expected error")
	}
}
