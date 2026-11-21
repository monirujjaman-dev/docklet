package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/auth"
	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "docklet ") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestRunNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

func TestRunUnknown(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func seedUser(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h, _ := auth.HashPassword("original password")
	if _, err := st.Q.CreateUser(ctx, db.CreateUserParams{Username: "admin", PasswordHash: h, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestResetPasswordAndVerify(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir)

	var out, errb bytes.Buffer
	if code := run([]string{"admin", "reset-password", "-data-dir", dir, "-username", "admin"}, &out, &errb); code != 0 {
		t.Fatalf("reset: exit %d, %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "New password: ") {
		t.Fatalf("no generated password in output: %q", out.String())
	}

	out.Reset()
	if code := run([]string{"audit", "verify", "-data-dir", dir}, &out, &errb); code != 0 {
		t.Fatalf("verify: exit %d, %s %s", code, out.String(), errb.String())
	}
	if !strings.HasPrefix(out.String(), "OK: 1 audit entries") {
		t.Fatalf("verify output %q", out.String())
	}

	// Tamper with the log directly; verify must fail.
	st, err := store.Open(context.Background(), dbPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"DROP TRIGGER audit_log_no_update", "UPDATE audit_log SET actor = 'x' WHERE id = 1"} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	out.Reset()
	if code := run([]string{"audit", "verify", "-data-dir", dir}, &out, &errb); code != 1 || !strings.Contains(out.String(), "BROKEN") {
		t.Fatalf("tampered verify: exit %d, %q", code, out.String())
	}
}

func TestResetPasswordUnknownUserAndMissingDB(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := run([]string{"admin", "reset-password", "-data-dir", dir, "-username", "admin"}, &out, &errb); code != 1 {
		t.Fatalf("missing db: exit %d", code)
	}
	seedUser(t, dir)
	errb.Reset()
	if code := run([]string{"admin", "reset-password", "-data-dir", dir, "-username", "ghost"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "not found") {
		t.Fatalf("unknown user: exit %d, %q", code, errb.String())
	}
}

func TestClearPanelDomain(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir)
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"panel_domain": "p.example.com", "panel_status": "active"} {
		if err := st.Q.PutSetting(ctx, db.PutSettingParams{Key: k, Value: v, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"admin", "clear-panel-domain", "-data-dir", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	st, _ = store.Open(ctx, dbPath(dir))
	defer func() { _ = st.Close() }()
	if _, err := st.Q.GetSetting(ctx, "panel_domain"); err == nil {
		t.Fatal("panel_domain still set")
	}
	var action string
	_ = st.DB.QueryRow("SELECT action FROM audit_log ORDER BY id DESC LIMIT 1").Scan(&action)
	if action != "settings.panel_domain_cleared" {
		t.Fatalf("last audit action = %q", action)
	}
}

func TestParsePrefixes(t *testing.T) {
	got, err := parsePrefixes(" 172.30.0.2, 10.0.0.0/8 ,")
	if err != nil || len(got) != 2 || got[0].String() != "172.30.0.2/32" || got[1].String() != "10.0.0.0/8" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := parsePrefixes("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestBackupRestoreCLI(t *testing.T) {
	src := t.TempDir()
	st, err := store.Open(context.Background(), dbPath(src))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Q.CreateProject(context.Background(), db.CreateProjectParams{Name: "keepme", CreatedAt: 1})
	_ = st.Close()
	_ = os.WriteFile(keyPath(src), bytes.Repeat([]byte{4}, 32), 0o600)

	var out, errb bytes.Buffer
	if code := run([]string{"backup", "-data-dir", src}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "DOCKLET_BACKUP_PASSPHRASE") {
		t.Fatalf("unencrypted without -no-encrypt: %d %s", code, errb.String())
	}
	t.Setenv("DOCKLET_BACKUP_PASSPHRASE", "short")
	if code := run([]string{"backup", "-data-dir", src}, &out, &errb); code != 2 {
		t.Fatalf("short passphrase accepted: %d", code)
	}
	t.Setenv("DOCKLET_BACKUP_PASSPHRASE", "a long enough passphrase")
	file := filepath.Join(t.TempDir(), "b.dkbackup")
	out.Reset()
	if code := run([]string{"backup", "-data-dir", src, "-o", file}, &out, &errb); code != 0 || !strings.Contains(out.String(), "encrypted") {
		t.Fatalf("backup: %d %s %s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"restore", "-check", "-i", file}, &out, &errb); code != 0 || !strings.Contains(out.String(), "checksums match") {
		t.Fatalf("check: %d %s %s", code, out.String(), errb.String())
	}

	dst := t.TempDir()
	unlock, err := selfbackup.Lock(dst) // as a running server would
	if err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := run([]string{"restore", "-data-dir", dst, "-i", file}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "Docklet running") {
		t.Fatalf("restore under a running server: %d %s", code, errb.String())
	}
	unlock()
	out.Reset()
	if code := run([]string{"restore", "-data-dir", dst, "-i", file}, &out, &errb); code != 0 {
		t.Fatalf("restore: %d %s", code, errb.String())
	}
	rs, err := store.Open(context.Background(), dbPath(dst))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	if ps, _ := rs.Q.ListProjects(context.Background()); len(ps) != 1 || ps[0].Name != "keepme" {
		t.Fatalf("projects = %v", ps)
	}
}
