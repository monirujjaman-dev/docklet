package selfbackup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

func newStore(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, dbName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := os.WriteFile(filepath.Join(dir, keyName), bytes.Repeat([]byte{9}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, _ := st.Q.CreateProject(ctx, db.CreateProjectParams{Name: "shop", CreatedAt: 1})
	// One database with two backups; only the newest is exported.
	bdir := filepath.Join(dir, "backups")
	for i, name := range []string{"old.dump", "new.dump"} {
		_ = os.MkdirAll(filepath.Join(bdir, "1"), 0o700)
		_ = os.WriteFile(filepath.Join(bdir, "1", name), []byte("PGDMP"+name), 0o600)
		_ = i
	}
	if _, err := st.DB.Exec(`INSERT INTO databases (id, project_id, name, engine, version, db_name, username, password_enc, root_password_enc, created_at, updated_at)
		VALUES (1, ?, 'main', 'postgres', '17-alpine', 'main', 'main', x'00', x'', 1, 1)`, p.ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"1/old.dump", "1/new.dump"} {
		if _, err := st.DB.Exec(`INSERT INTO backups (database_id, trigger, status, file, size_bytes, started_at, finished_at) VALUES (1, 'manual', 'succeeded', ?, 10, 1, 2)`, f); err != nil {
			t.Fatal(err)
		}
	}
	return st, dir, bdir
}

func TestRoundTripEncrypted(t *testing.T) {
	st, dir, bdir := newStore(t)
	var buf bytes.Buffer
	m, err := Write(context.Background(), &buf, st, dir, Options{Passphrase: "correct horse battery", BackupDir: bdir})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Encrypted || m.DatabaseBackups != 1 || m.SchemaVersion != LatestMigration() || len(m.Files) != 3 {
		t.Fatalf("manifest = %+v", m)
	}
	if !bytes.HasPrefix(buf.Bytes(), magic) || bytes.Contains(buf.Bytes(), []byte("PGDMP")) {
		t.Fatal("archive is not encrypted")
	}
	if _, err := Inspect(bytes.NewReader(buf.Bytes()), ""); !errors.Is(err, ErrNeedPassphrase) {
		t.Fatalf("no passphrase: %v", err)
	}
	if _, err := Inspect(bytes.NewReader(buf.Bytes()), "wrong"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Inspect(bytes.NewReader(buf.Bytes()), "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	// Restore into a fresh data dir.
	dst := t.TempDir()
	dstBackups := filepath.Join(dst, "backups")
	_ = os.WriteFile(filepath.Join(dst, keyName), []byte("old key"), 0o600)
	got, aside, err := Restore(bytes.NewReader(buf.Bytes()), dst, dstBackups, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseBackups != 1 {
		t.Fatalf("restored manifest = %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(aside, keyName)); string(b) != "old key" { //nolint:gosec // test paths
		t.Fatal("previous key not kept aside")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, keyName)); !bytes.Equal(b, bytes.Repeat([]byte{9}, 32)) { //nolint:gosec // test paths
		t.Fatal("key not restored")
	}
	if b, _ := os.ReadFile(filepath.Join(dstBackups, "1", "new.dump")); string(b) != "PGDMPnew.dump" { //nolint:gosec // test paths
		t.Fatal("database backup not restored")
	}
	if _, err := os.Stat(filepath.Join(dstBackups, "1", "old.dump")); err == nil {
		t.Fatal("old database backup should not be exported")
	}
	rs, err := store.Open(context.Background(), filepath.Join(dst, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	if ps, _ := rs.Q.ListProjects(context.Background()); len(ps) != 1 || ps[0].Name != "shop" {
		t.Fatalf("projects after restore = %v", ps)
	}
}

func TestPlainArchiveAndTampering(t *testing.T) {
	st, dir, _ := newStore(t)
	var buf bytes.Buffer
	if _, err := Write(context.Background(), &buf, st, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[0] != 0x1f {
		t.Fatal("plain backup should be gzip")
	}
	if m, err := Inspect(bytes.NewReader(buf.Bytes()), ""); err != nil || m.Encrypted || m.DatabaseBackups != 0 {
		t.Fatalf("inspect = %+v, %v", m, err)
	}

	var enc bytes.Buffer
	_, _ = Write(context.Background(), &enc, st, dir, Options{Passphrase: "pass phrase 123"})
	data := enc.Bytes()
	for name, mutate := range map[string]func([]byte) []byte{
		"truncated":    func(b []byte) []byte { return b[:len(b)-40] },
		"bit flip":     func(b []byte) []byte { c := bytes.Clone(b); c[len(c)/2] ^= 1; return c },
		"header flip":  func(b []byte) []byte { c := bytes.Clone(b); c[40] ^= 1; return c },
		"extra bytes":  func(b []byte) []byte { return append(bytes.Clone(b), 0, 0, 0, 0) },
		"last dropped": func(b []byte) []byte { return b[:headerSize] },
	} {
		if _, err := Inspect(bytes.NewReader(mutate(data)), "pass phrase 123"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Inspect(strings.NewReader("hello world, not a backup"), ""); err == nil {
		t.Error("garbage accepted")
	}
}

func TestRestoreRejectsNewerSchemaAndKeepsFiles(t *testing.T) {
	st, dir, _ := newStore(t)
	if _, err := st.DB.Exec("INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, 1)", LatestMigration()+5); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := Write(context.Background(), &buf, st, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	_ = os.WriteFile(filepath.Join(dst, keyName), []byte("keep me"), 0o600)
	if _, _, err := Restore(bytes.NewReader(buf.Bytes()), dst, "", ""); err == nil || !strings.Contains(err.Error(), "newer Docklet") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, keyName)); string(b) != "keep me" { //nolint:gosec // test paths
		t.Fatal("failed restore touched the data dir")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	again()
}
