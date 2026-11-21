package databases

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Backup triggers.
const (
	TriggerManual     = "manual"
	TriggerSchedule   = "schedule"
	TriggerPreRestore = "pre-restore"
)

// Schedules and their intervals.
var Schedules = map[string]time.Duration{
	"off": 0, "hourly": time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour,
}

func (s *Service) backupPath(rel string) string { return filepath.Join(s.BackupDir, rel) }

func (s *Service) removeBackupDir(id int64) {
	_ = os.RemoveAll(filepath.Join(s.BackupDir, fmt.Sprint(id)))
}

// Backup dumps a running database to a file under BackupDir.
func (s *Service) Backup(ctx context.Context, id int64, trigger string) (db.Backup, error) {
	defer s.lock(id)()
	return s.backup(ctx, id, trigger)
}

func (s *Service) backup(ctx context.Context, id int64, trigger string) (db.Backup, error) {
	d, err := s.st.Q.GetDatabase(ctx, id)
	if err != nil {
		return db.Backup{}, err
	}
	b, err := s.st.Q.CreateBackup(ctx, db.CreateBackupParams{DatabaseID: id, Trigger: trigger, StartedAt: s.now().Unix()})
	if err != nil {
		return b, err
	}
	rel, size, derr := s.dump(ctx, d, b)
	status, msg := "succeeded", ""
	if derr != nil {
		status, msg = "failed", derr.Error()
		if rel != "" {
			_ = os.Remove(s.backupPath(rel))
			rel = ""
		}
	}
	if err := s.st.Q.FinishBackup(context.WithoutCancel(ctx), db.FinishBackupParams{
		Status: status, File: rel, SizeBytes: size, Error: msg, FinishedAt: s.now().Unix(), ID: b.ID,
	}); err != nil {
		return b, err
	}
	b, _ = s.st.Q.GetBackup(context.WithoutCancel(ctx), b.ID)
	action := audit.ActionBackupSucceeded
	details := map[string]any{"backup": b.ID, "trigger": trigger, "bytes": size}
	if derr != nil {
		action, details["error"] = audit.ActionBackupFailed, msg
	}
	s.auditSystem(ctx, d, action, details)
	if derr != nil {
		return b, derr
	}
	if err := s.upload(ctx, d, b); err != nil {
		s.log.Warn("upload backup", "database", d.Name, "backup", b.ID, "err", err)
	}
	b, _ = s.st.Q.GetBackup(context.WithoutCancel(ctx), b.ID)
	s.prune(ctx, d)
	return b, nil
}

// dump writes the backup file and returns its relative path and size.
func (s *Service) dump(ctx context.Context, d db.Database, b db.Backup) (string, int64, error) {
	e := Engines[d.Engine]
	if d.ContainerID == "" {
		return "", 0, errors.New("the database has no container")
	}
	c, err := s.Creds(d)
	if err != nil {
		return "", 0, err
	}
	dir := filepath.Join(s.BackupDir, fmt.Sprint(d.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	rel := filepath.Join(fmt.Sprint(d.ID), fmt.Sprintf("%s-%d%s", s.now().UTC().Format("20060102-150405"), b.ID, e.BackupExt))
	f, err := os.OpenFile(s.backupPath(rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- path built from ids
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	counter := &countWriter{w: f}

	switch d.Engine {
	case "postgres":
		err = s.execTo(ctx, d.ContainerID, []string{"pg_dump", "-Fc", "-h", "127.0.0.1", "-U", c.User, c.DBName},
			[]string{"PGPASSWORD=" + c.Password}, counter)
	case "mysql", "mariadb":
		bin := "mysqldump"
		if d.Engine == "mariadb" {
			bin = "mariadb-dump"
		}
		gz := gzip.NewWriter(counter)
		err = s.execTo(ctx, d.ContainerID, []string{bin, "-h", "127.0.0.1", "-uroot", "--single-transaction",
			"--routines", "--triggers", "--events", "--databases", c.DBName}, []string{"MYSQL_PWD=" + c.RootPassword}, gz)
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
	case "redis":
		const tmp = "/tmp/docklet-backup.rdb"
		if err = s.execTo(ctx, d.ContainerID, []string{"redis-cli", "--no-auth-warning", "--rdb", tmp},
			[]string{"REDISCLI_AUTH=" + c.Password}, io.Discard); err == nil {
			gz := gzip.NewWriter(counter)
			_, err = s.dk.CopyFrom(ctx, d.ContainerID, tmp, gz)
			if cerr := gz.Close(); err == nil {
				err = cerr
			}
			_, _ = s.dk.Exec(ctx, d.ContainerID, []string{"rm", "-f", tmp}, nil, nil)
		}
	default:
		err = fmt.Errorf("backups are not supported for %s", d.Engine)
	}
	if err != nil {
		return rel, counter.n, err
	}
	if err := f.Sync(); err != nil {
		return rel, counter.n, err
	}
	if counter.n == 0 {
		return rel, 0, errors.New("the dump is empty")
	}
	return rel, counter.n, nil
}

// execTo runs cmd and fails on a non-zero exit with the command's stderr.
func (s *Service) execTo(ctx context.Context, id string, cmd, env []string, w io.Writer) error {
	res, err := s.dk.Exec(ctx, id, cmd, env, w)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s exited with %d: %s", cmd[0], res.ExitCode, lastLine(res.Stderr))
	}
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// prune deletes successful backups beyond the database's keep count.
func (s *Service) prune(ctx context.Context, d db.Database) {
	if d.BackupKeep <= 0 {
		return
	}
	old, err := s.st.Q.ListExpiredBackups(ctx, db.ListExpiredBackupsParams{DatabaseID: d.ID, Keep: d.BackupKeep})
	if err != nil {
		return
	}
	for _, b := range old {
		if b.File != "" {
			_ = os.Remove(s.backupPath(b.File))
		}
		s.deleteRemote(ctx, b)
		_ = s.st.Q.DeleteBackup(ctx, b.ID)
	}
}

// Restore replaces the database's contents with a backup. A pre-restore
// backup is taken first so the restore itself can be undone.
func (s *Service) Restore(ctx context.Context, id, backupID int64) (pre db.Backup, err error) {
	defer s.lock(id)()
	b, err := s.st.Q.GetBackup(ctx, backupID)
	if err != nil || b.DatabaseID != id {
		return pre, errors.New("backup not found for this database")
	}
	if b.Status != "succeeded" {
		return pre, errors.New("only successful backups can be restored")
	}
	pre, err = s.backup(ctx, id, TriggerPreRestore)
	if err != nil {
		return pre, fmt.Errorf("safety backup before restore failed, nothing was changed: %w", err)
	}
	d, err := s.st.Q.GetDatabase(ctx, id)
	if err != nil {
		return pre, err
	}
	cleanup, rerr := s.fetchLocal(ctx, b)
	if rerr == nil {
		rerr = s.load(ctx, d, b)
		cleanup()
	}
	action, details := audit.ActionRestoreSucceeded, map[string]any{"backup": b.ID, "preRestoreBackup": pre.ID}
	if rerr != nil {
		action, details["error"] = audit.ActionRestoreFailed, rerr.Error()
	}
	s.auditSystem(ctx, d, action, details)
	return pre, rerr
}

func (s *Service) load(ctx context.Context, d db.Database, b db.Backup) error {
	c, err := s.Creds(d)
	if err != nil {
		return err
	}
	f, err := os.Open(s.backupPath(b.File)) // #nosec G304 -- path from our own row
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var data io.Reader = f
	if strings.HasSuffix(b.File, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		data = gz
	}
	// The archive API needs a size: buffer to a temp file.
	tmp, err := os.CreateTemp(s.BackupDir, "restore-*")
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	size, err := io.Copy(tmp, data)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	switch d.Engine {
	case "postgres":
		const path = "/tmp/docklet-restore.dump"
		if err := s.dk.CopyTo(ctx, d.ContainerID, "/tmp", "docklet-restore.dump", tmp, size, 0o644); err != nil {
			return err
		}
		defer func() {
			_, _ = s.dk.Exec(context.WithoutCancel(ctx), d.ContainerID, []string{"rm", "-f", path}, nil, nil)
		}()
		env := []string{"PGPASSWORD=" + c.Password}
		// Recreate the database if it was dropped; "already exists" is fine.
		var out bytes.Buffer
		_, _ = s.dk.Exec(ctx, d.ContainerID, []string{"psql", "-h", "127.0.0.1", "-U", c.User, "-d", "postgres",
			"-c", fmt.Sprintf("CREATE DATABASE %q", c.DBName)}, env, &out)
		return s.execTo(ctx, d.ContainerID, []string{"pg_restore", "--clean", "--if-exists", "--no-owner",
			"-h", "127.0.0.1", "-U", c.User, "-d", c.DBName, path}, env, io.Discard)
	case "mysql", "mariadb":
		const path = "/tmp/docklet-restore.sql"
		if err := s.dk.CopyTo(ctx, d.ContainerID, "/tmp", "docklet-restore.sql", tmp, size, 0o644); err != nil {
			return err
		}
		defer func() {
			_, _ = s.dk.Exec(context.WithoutCancel(ctx), d.ContainerID, []string{"rm", "-f", path}, nil, nil)
		}()
		bin := "mysql"
		if d.Engine == "mariadb" {
			bin = "mariadb"
		}
		return s.execTo(ctx, d.ContainerID, []string{"sh", "-c", bin + " -h 127.0.0.1 -uroot < " + path},
			[]string{"MYSQL_PWD=" + c.RootPassword}, io.Discard)
	case "redis":
		// Replace dump.rdb, reload it in place, then rewrite the AOF from memory.
		if err := s.dk.CopyTo(ctx, d.ContainerID, "/data", "dump.rdb", tmp, size, 0o644); err != nil {
			return err
		}
		env := []string{"REDISCLI_AUTH=" + c.Password}
		if err := s.execTo(ctx, d.ContainerID, []string{"redis-cli", "--no-auth-warning", "DEBUG", "RELOAD", "NOSAVE"}, env, io.Discard); err != nil {
			return err
		}
		if err := s.execTo(ctx, d.ContainerID, []string{"redis-cli", "--no-auth-warning", "BGREWRITEAOF"}, env, io.Discard); err != nil {
			return err
		}
		// The rewrite runs in the background. Until it finishes, the AOF on
		// disk still ends with whatever was there before (e.g. a FLUSHALL),
		// so a restart would bring that state back. Wait for it.
		return s.waitAOFRewrite(ctx, d.ContainerID, env)
	}
	return fmt.Errorf("restore is not supported for %s", d.Engine)
}

// waitAOFRewrite polls INFO persistence until no AOF rewrite is running or
// scheduled, and reports a failed rewrite.
func (s *Service) waitAOFRewrite(ctx context.Context, id string, env []string) error {
	deadline := s.now().Add(s.ReadyTimeout)
	for {
		var out strings.Builder
		if err := s.execTo(ctx, id, []string{"redis-cli", "--no-auth-warning", "INFO", "persistence"}, env, &out); err != nil {
			return err
		}
		info := map[string]string{}
		for _, line := range strings.Split(out.String(), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
				info[k] = v
			}
		}
		if info["aof_rewrite_in_progress"] == "0" && info["aof_rewrite_scheduled"] == "0" {
			if st := info["aof_last_bgrewrite_status"]; st != "" && st != "ok" {
				return fmt.Errorf("redis AOF rewrite failed (%s)", st)
			}
			return nil
		}
		if s.now().After(deadline) {
			return errors.New("timed out waiting for the redis AOF rewrite")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.ReadyInterval):
		}
	}
}

// RunScheduled takes due scheduled backups, one database at a time.
func (s *Service) RunScheduled(ctx context.Context) {
	list, err := s.st.Q.ListDatabases(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, d := range list {
		every := Schedules[d.BackupSchedule]
		if every == 0 || d.RuntimeStatus != StatusRunning {
			continue
		}
		last, err := s.st.Q.LastBackupTime(ctx, d.ID)
		if err != nil || now.Sub(time.Unix(last, 0)) < every {
			continue
		}
		if _, err := s.Backup(ctx, d.ID, TriggerSchedule); err != nil {
			s.log.Warn("scheduled backup failed", "db", d.Name, "err", err)
		}
	}
}

// BackupAsync runs a backup in the background.
func (s *Service) BackupAsync(id int64, trigger string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if _, err := s.Backup(ctx, id, trigger); err != nil {
			s.log.Warn("backup failed", "database", id, "err", err)
		}
	}()
}

// RestoreAsync restores in the background, showing progress in the
// database's runtime message.
func (s *Service) RestoreAsync(id, backupID int64) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		s.setRuntime(ctx, id, StatusRunning, fmt.Sprintf("Restoring backup #%d…", backupID))
		if _, err := s.Restore(ctx, id, backupID); err != nil {
			s.setRuntime(ctx, id, StatusRunning, "Restore failed: "+err.Error())
			return
		}
		s.setRuntime(ctx, id, StatusRunning, "")
	}()
}
