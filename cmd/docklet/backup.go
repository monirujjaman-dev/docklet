package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
	"github.com/monirujjaman-dev/docklet/internal/store"
)

const minPassphrase = 12

// passphrase reads -passphrase-file or DOCKLET_BACKUP_PASSPHRASE.
func passphrase(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file) //nolint:gosec // path given by the operator
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv("DOCKLET_BACKUP_PASSPHRASE"), nil
}

func cmdBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	out := fs.String("o", "", "output file, or - for stdout (default: <data-dir>/self-backups/docklet-<time>.dkbackup)")
	passFile := fs.String("passphrase-file", "", "file holding the encryption passphrase (or env DOCKLET_BACKUP_PASSPHRASE)")
	noEncrypt := fs.Bool("no-encrypt", false, "write a plain .tar.gz; it contains the master key that decrypts every secret")
	withDBs := fs.Bool("with-database-backups", false, "include the newest backup of each managed database")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: docklet backup [flags]

Writes Docklet's database and master key (and optionally the newest backup of
each managed database) to one archive. Safe while Docklet is running.

  DOCKLET_BACKUP_PASSPHRASE='…' docklet backup -o /backups/docklet.dkbackup
  docker compose exec -T -e DOCKLET_BACKUP_PASSPHRASE='…' docklet docklet backup -o - > docklet.dkbackup

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pass, err := passphrase(*passFile)
	if err != nil {
		fmt.Fprintf(stderr, "passphrase: %v\n", err)
		return 1
	}
	switch {
	case pass == "" && !*noEncrypt:
		fmt.Fprintln(stderr, "set DOCKLET_BACKUP_PASSPHRASE or -passphrase-file (the archive holds the master key), or pass -no-encrypt")
		return 2
	case pass != "" && *noEncrypt:
		fmt.Fprintln(stderr, "-no-encrypt conflicts with a passphrase")
		return 2
	case pass != "" && len(pass) < minPassphrase:
		fmt.Fprintf(stderr, "the passphrase must be at least %d characters\n", minPassphrase)
		return 2
	}

	ctx := context.Background()
	st, err := store.Open(ctx, dbPath(*dataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open database: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	w := stdout
	target := *out
	var f *os.File
	if target != "-" {
		if target == "" {
			target = filepath.Join(*dataDir, "self-backups", "docklet-"+time.Now().UTC().Format("20060102-150405")+".dkbackup")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		tmp := target + ".partial"
		if f, err = os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err != nil { //nolint:gosec // operator-chosen path
			fmt.Fprintln(stderr, err)
			return 1
		}
		w = f
	}
	opt := selfbackup.Options{Passphrase: pass}
	if *withDBs {
		opt.BackupDir = filepath.Join(*dataDir, "backups")
	}
	m, err := selfbackup.Write(ctx, w, st, *dataDir, opt)
	if f != nil {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(f.Name(), target)
		} else {
			_ = os.Remove(f.Name())
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "backup failed: %v\n", err)
		return 1
	}
	if target != "-" {
		enc := "encrypted"
		if !m.Encrypted {
			enc = "NOT encrypted: keep it somewhere safe"
		}
		fmt.Fprintf(stdout, "Wrote %s (%s, schema %d, %d database backup(s))\n", target, enc, m.SchemaVersion, m.DatabaseBackups)
	}
	return 0
}

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	in := fs.String("i", "", "backup file to restore, or - for stdin")
	passFile := fs.String("passphrase-file", "", "file holding the passphrase (or env DOCKLET_BACKUP_PASSPHRASE)")
	check := fs.Bool("check", false, "only verify the archive and print what it contains")
	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: docklet restore -i <file> [flags]

Replaces Docklet's database and master key with those in a backup. Docklet
must be stopped. The current files are moved to <data-dir>/pre-restore-<time>/.

  docker compose stop docklet
  docker compose run --rm --no-deps -e DOCKLET_BACKUP_PASSPHRASE='…' \
    -v "$PWD/docklet.dkbackup:/restore.dkbackup:ro" docklet restore -i /restore.dkbackup
  docker compose start docklet

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" {
		fs.Usage()
		return 2
	}
	pass, err := passphrase(*passFile)
	if err != nil {
		fmt.Fprintf(stderr, "passphrase: %v\n", err)
		return 1
	}
	var r io.Reader = os.Stdin
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	describe := func(m selfbackup.Manifest) {
		fmt.Fprintf(stdout, "Backup from %s, %s (Docklet %s, schema %d), encrypted: %v, database backups: %d\n",
			m.Hostname, m.CreatedAt.Format(time.RFC3339), m.DockletVersion, m.SchemaVersion, m.Encrypted, m.DatabaseBackups)
	}
	if *check {
		m, err := selfbackup.Inspect(r, pass)
		if err != nil {
			fmt.Fprintf(stderr, "invalid backup: %v\n", err)
			return 1
		}
		describe(m)
		fmt.Fprintln(stdout, "OK: all checksums match")
		return 0
	}

	unlock, err := selfbackup.Lock(*dataDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer unlock()
	m, aside, err := selfbackup.Restore(r, *dataDir, filepath.Join(*dataDir, "backups"), pass)
	if err != nil {
		if errors.Is(err, selfbackup.ErrNeedPassphrase) {
			err = fmt.Errorf("%w (set DOCKLET_BACKUP_PASSPHRASE or -passphrase-file)", err)
		}
		fmt.Fprintf(stderr, "restore failed: %v\n", err)
		if aside != "" {
			fmt.Fprintf(stderr, "your previous files are in %s\n", aside)
		}
		return 1
	}
	describe(m)
	fmt.Fprintf(stdout, "Restored. Previous files were moved to %s.\n", aside)
	fmt.Fprintln(stdout, "Start Docklet: apps whose containers no longer exist show as \"missing\"; redeploy them, and recreate databases, then restore their data from Backups.")
	return 0
}
