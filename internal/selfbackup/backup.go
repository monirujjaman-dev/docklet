// Package selfbackup exports and restores Docklet's own state: the SQLite
// database, the master key that decrypts its secrets, and optionally the
// newest backup of each managed database.
package selfbackup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/store"
	"github.com/monirujjaman-dev/docklet/internal/version"
	"github.com/monirujjaman-dev/docklet/migrations"
)

// FormatVersion is the archive layout version.
const FormatVersion = 1

// File names inside the archive.
const (
	manifestName = "manifest.json"
	dbName       = "docklet.db"
	keyName      = "master.key"
	backupsDir   = "database-backups/"
)

// Manifest describes an archive.
type Manifest struct {
	Format          int               `json:"format"`
	DockletVersion  string            `json:"dockletVersion"`
	DockletCommit   string            `json:"dockletCommit"`
	SchemaVersion   int64             `json:"schemaVersion"`
	CreatedAt       time.Time         `json:"createdAt"`
	Hostname        string            `json:"hostname"`
	Encrypted       bool              `json:"encrypted"`
	Files           map[string]string `json:"files"` // name -> sha256
	DatabaseBackups int               `json:"databaseBackups"`
}

// Options for Write.
type Options struct {
	Passphrase string // empty = unencrypted .tar.gz
	// BackupDir, when set, includes the newest successful backup of every
	// managed database (paths relative to BackupDir, as stored in the DB).
	BackupDir string
}

// SchemaVersion returns the newest migration applied to st.
func SchemaVersion(ctx context.Context, st *store.Store) (int64, error) {
	var v sql.NullInt64
	err := st.DB.QueryRowContext(ctx, "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1").Scan(&v)
	return v.Int64, err
}

// LatestMigration is the newest migration this binary knows.
func LatestMigration() int64 {
	var latest int64
	entries, _ := fs.ReadDir(migrations.FS, ".")
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if n, err := strconv.ParseInt(num, 10, 64); ok && err == nil && n > latest {
			latest = n
		}
	}
	return latest
}

// Write streams a backup of the live store to w. The database is copied with
// VACUUM INTO, which gives a consistent snapshot while Docklet keeps running.
func Write(ctx context.Context, w io.Writer, st *store.Store, dataDir string, opt Options) (Manifest, error) {
	m := Manifest{
		Format: FormatVersion, DockletVersion: version.Version, DockletCommit: version.Commit,
		CreatedAt: time.Now().UTC().Truncate(time.Second), Encrypted: opt.Passphrase != "", Files: map[string]string{},
	}
	m.Hostname, _ = os.Hostname()
	var err error
	if m.SchemaVersion, err = SchemaVersion(ctx, st); err != nil {
		return m, fmt.Errorf("schema version: %w", err)
	}

	tmp, err := os.MkdirTemp(dataDir, ".selfbackup-")
	if err != nil {
		return m, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	snap := filepath.Join(tmp, dbName)
	if _, err := st.DB.ExecContext(ctx, "VACUUM INTO ?", snap); err != nil {
		return m, fmt.Errorf("snapshot database: %w", err)
	}

	files := []struct{ name, path string }{{dbName, snap}, {keyName, filepath.Join(dataDir, keyName)}}
	if opt.BackupDir != "" {
		rows, err := st.DB.QueryContext(ctx, `
			SELECT b.file FROM backups b
			WHERE b.status = 'succeeded' AND b.file <> '' AND b.id = (
				SELECT MAX(b2.id) FROM backups b2 WHERE b2.database_id = b.database_id AND b2.status = 'succeeded')`)
		if err != nil {
			return m, err
		}
		for rows.Next() {
			var rel string
			if err := rows.Scan(&rel); err != nil {
				_ = rows.Close()
				return m, err
			}
			rel = filepath.ToSlash(filepath.Clean(rel))
			if !fs.ValidPath(rel) {
				continue
			}
			p := filepath.Join(opt.BackupDir, filepath.FromSlash(rel))
			if _, err := os.Stat(p); err == nil {
				files = append(files, struct{ name, path string }{backupsDir + rel, p})
				m.DatabaseBackups++
			}
		}
		_ = rows.Close()
	}
	for _, f := range files {
		sum, err := fileSHA256(f.path)
		if err != nil {
			return m, fmt.Errorf("read %s: %w", f.name, err)
		}
		m.Files[f.name] = sum
	}

	out := w
	var sealer *sealWriter
	if opt.Passphrase != "" {
		if sealer, err = newSealWriter(w, opt.Passphrase); err != nil {
			return m, err
		}
		out = sealer
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	mj, _ := json.MarshalIndent(m, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mj)), ModTime: m.CreatedAt}); err != nil {
		return m, err
	}
	if _, err := tw.Write(mj); err != nil {
		return m, err
	}
	for _, f := range files {
		if err := addFile(tw, f.name, f.path, m.CreatedAt); err != nil {
			return m, err
		}
	}
	if err := tw.Close(); err != nil {
		return m, err
	}
	if err := gz.Close(); err != nil {
		return m, err
	}
	if sealer != nil {
		return m, sealer.Close()
	}
	return m, nil
}

func addFile(tw *tar.Writer, name, p string, mod time.Time) error {
	f, err := os.Open(p) //nolint:gosec // our own data files
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: mod}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // our own data files
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// open detects an encrypted or plain archive and returns the tar stream.
func open(r io.Reader, pass string) (*tar.Reader, bool, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, err := br.Peek(len(magic))
	if err != nil {
		return nil, false, errors.New("not a Docklet backup (file too short)")
	}
	var src io.Reader = br
	encrypted := bytes.Equal(head, magic)
	if encrypted {
		if pass == "" {
			return nil, true, ErrNeedPassphrase
		}
		if src, err = newOpenReader(br, pass); err != nil {
			return nil, true, err
		}
	} else if head[0] != 0x1f || head[1] != 0x8b {
		return nil, false, errors.New("not a Docklet backup")
	}
	gz, err := gzip.NewReader(src)
	if err != nil {
		if errors.Is(err, ErrPassphrase) {
			return nil, encrypted, err
		}
		return nil, encrypted, fmt.Errorf("not a Docklet backup: %w", err)
	}
	return tar.NewReader(gz), encrypted, nil
}

// Restore unpacks an archive into dataDir and backupDir. Docklet must not be
// running: the caller holds the data-dir lock. The current database and key
// are moved to <dataDir>/pre-restore-<time>/ first.
func Restore(r io.Reader, dataDir, backupDir, pass string) (Manifest, string, error) {
	var m Manifest
	tr, encrypted, err := open(r, pass)
	if err != nil {
		return m, "", err
	}
	stage, err := os.MkdirTemp(dataDir, ".restore-")
	if err != nil {
		return m, "", err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	sums := map[string]string{}
	first := true
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, "", fmt.Errorf("read archive: %w", err)
		}
		if first {
			if h.Name != manifestName {
				return m, "", errors.New("not a Docklet backup (no manifest)")
			}
			first = false
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
				return m, "", fmt.Errorf("manifest: %w", err)
			}
			if m.Format != FormatVersion {
				return m, "", fmt.Errorf("unsupported backup format %d", m.Format)
			}
			if m.SchemaVersion > LatestMigration() {
				return m, "", fmt.Errorf("the backup is from a newer Docklet (%s, schema %d); upgrade this Docklet first", m.DockletVersion, m.SchemaVersion)
			}
			continue
		}
		want, ok := m.Files[h.Name]
		if !ok || h.Typeflag != tar.TypeReg || !fs.ValidPath(h.Name) {
			return m, "", fmt.Errorf("unexpected entry %q in the backup", h.Name)
		}
		dst := filepath.Join(stage, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return m, "", err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) //nolint:gosec // validated relative path
		if err != nil {
			return m, "", err
		}
		hs := sha256.New()
		_, err = io.Copy(io.MultiWriter(f, hs), tr) //nolint:gosec // size bounded by the archive we were given
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return m, "", fmt.Errorf("extract %s: %w", h.Name, err)
		}
		if got := hex.EncodeToString(hs.Sum(nil)); got != want {
			return m, "", fmt.Errorf("%s is corrupted (checksum mismatch)", h.Name)
		}
		sums[h.Name] = want
	}
	if first {
		return m, "", errors.New("the backup is empty")
	}
	for name := range m.Files {
		if _, ok := sums[name]; !ok {
			return m, "", fmt.Errorf("the backup is incomplete: %s is missing", name)
		}
	}
	m.Encrypted = encrypted
	if err := checkDatabase(filepath.Join(stage, dbName)); err != nil {
		return m, "", err
	}

	// Swap in: keep the current files aside, then move the restored ones.
	aside := filepath.Join(dataDir, "pre-restore-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return m, "", err
	}
	for _, n := range []string{dbName, dbName + "-wal", dbName + "-shm", keyName} {
		if err := os.Rename(filepath.Join(dataDir, n), filepath.Join(aside, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return m, aside, err
		}
	}
	for _, n := range []string{dbName, keyName} {
		if err := os.Rename(filepath.Join(stage, n), filepath.Join(dataDir, n)); err != nil {
			return m, aside, err
		}
	}
	for name := range m.Files {
		rel, ok := strings.CutPrefix(name, backupsDir)
		if !ok || backupDir == "" {
			continue
		}
		dst := filepath.Join(backupDir, filepath.FromSlash(path.Clean(rel)))
		if _, err := os.Stat(dst); err == nil {
			continue // already there
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return m, aside, err
		}
		if err := os.Rename(filepath.Join(stage, filepath.FromSlash(name)), dst); err != nil {
			return m, aside, err
		}
	}
	return m, aside, nil
}

// Inspect reads and verifies an archive without restoring it.
func Inspect(r io.Reader, pass string) (Manifest, error) {
	var m Manifest
	tr, encrypted, err := open(r, pass)
	if err != nil {
		return m, err
	}
	h, err := tr.Next()
	if err != nil || h.Name != manifestName {
		return m, errors.New("not a Docklet backup (no manifest)")
	}
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("read archive: %w", err)
		}
		hs := sha256.New()
		if _, err := io.Copy(hs, tr); err != nil { //nolint:gosec // hashing only
			return m, err
		}
		if hex.EncodeToString(hs.Sum(nil)) != m.Files[h.Name] {
			return m, fmt.Errorf("%s is corrupted (checksum mismatch)", h.Name)
		}
	}
	m.Encrypted = encrypted
	return m, nil
}

func checkDatabase(p string) error {
	d, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	var res string
	if err := d.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		return fmt.Errorf("the backed-up database can't be read: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("the backed-up database is damaged: %s", res)
	}
	return nil
}
