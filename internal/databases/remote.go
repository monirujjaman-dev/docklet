package databases

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/s3"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Remote statuses of a backup.
const (
	RemoteUploading = "uploading"
	RemoteUploaded  = "uploaded"
	RemoteFailed    = "failed"
)

// RemoteKey is where a backup is stored in its destination.
func RemoteKey(d db.Database, b db.Backup) string {
	return path.Join("databases", fmt.Sprintf("%d-%s", d.ID, d.Name), path.Base(filepath.ToSlash(b.File)))
}

// HasLocalFile reports whether the backup file is still on this server.
func (s *Service) HasLocalFile(b db.Backup) bool {
	if b.File == "" {
		return false
	}
	_, err := os.Stat(s.backupPath(b.File))
	return err == nil
}

// upload copies a finished backup to the database's destination, if it has
// one. A failed upload leaves the local backup in place.
func (s *Service) upload(ctx context.Context, d db.Database, b db.Backup) error {
	if !d.BackupDestinationID.Valid || s.Remote == nil || b.Status != "succeeded" || b.File == "" {
		return nil
	}
	dest := d.BackupDestinationID
	key := RemoteKey(d, b)
	set := func(status, msg string) {
		_ = s.st.Q.SetBackupRemote(context.WithoutCancel(ctx), db.SetBackupRemoteParams{
			DestinationID: dest, RemoteKey: key, RemoteStatus: status, RemoteError: msg, ID: b.ID,
		})
	}
	set(RemoteUploading, "")
	err := func() error {
		c, err := s.Remote(ctx, dest.Int64)
		if err != nil {
			return err
		}
		f, err := os.Open(s.backupPath(b.File)) // #nosec G304 -- path from our own row
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		uctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
		defer cancel()
		return c.Upload(uctx, key, f, fi.Size())
	}()
	details := map[string]any{"backup": b.ID, "destination": dest.Int64, "key": key}
	if err != nil {
		set(RemoteFailed, err.Error())
		details["error"] = err.Error()
		s.auditSystem(ctx, d, audit.ActionBackupUploadFailed, details)
		return err
	}
	set(RemoteUploaded, "")
	s.auditSystem(ctx, d, audit.ActionBackupUploaded, details)
	return nil
}

// UploadBackup retries the upload of a backup to the database's current
// destination.
func (s *Service) UploadBackup(ctx context.Context, backupID int64) error {
	b, err := s.st.Q.GetBackup(ctx, backupID)
	if err != nil {
		return err
	}
	defer s.lock(b.DatabaseID)()
	d, err := s.st.Q.GetDatabase(ctx, b.DatabaseID)
	if err != nil {
		return err
	}
	if !d.BackupDestinationID.Valid {
		return errors.New("this database has no backup destination")
	}
	if !s.HasLocalFile(b) {
		return errors.New("the backup file is no longer on this server")
	}
	return s.upload(ctx, d, b)
}

func (s *Service) deleteRemote(ctx context.Context, b db.Backup) {
	if b.RemoteStatus != RemoteUploaded || !b.DestinationID.Valid || s.Remote == nil {
		return
	}
	c, err := s.Remote(ctx, b.DestinationID.Int64)
	if err == nil {
		err = c.Delete(ctx, b.RemoteKey)
	}
	if err != nil {
		s.log.Warn("delete expired remote backup", "backup", b.ID, "key", b.RemoteKey, "err", err)
	}
}

// OpenBackupReader opens a backup from this server, or from its destination
// when the local file is gone.
func (s *Service) OpenBackupReader(ctx context.Context, b db.Backup) (io.ReadCloser, int64, error) {
	if b.Status != "succeeded" || b.File == "" {
		return nil, 0, errors.New("backup has no file")
	}
	if f, err := os.Open(s.backupPath(b.File)); err == nil { // #nosec G304 -- path from our own row
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, fi.Size(), nil
	}
	if b.RemoteStatus != RemoteUploaded || !b.DestinationID.Valid || s.Remote == nil {
		return nil, 0, errors.New("the backup file is not on this server and was not uploaded")
	}
	c, err := s.Remote(ctx, b.DestinationID.Int64)
	if err != nil {
		return nil, 0, err
	}
	body, n, err := c.Get(ctx, b.RemoteKey)
	if err != nil {
		if s3.IsNotFound(err) {
			return nil, 0, errors.New("the backup is missing from the bucket too")
		}
		return nil, 0, fmt.Errorf("download from the destination: %w", err)
	}
	return body, n, nil
}

// fetchLocal makes sure the backup file exists locally, downloading it from
// the destination if needed. cleanup removes a downloaded copy.
func (s *Service) fetchLocal(ctx context.Context, b db.Backup) (cleanup func(), err error) {
	if s.HasLocalFile(b) {
		return func() {}, nil
	}
	r, _, err := s.OpenBackupReader(ctx, b)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	p := s.backupPath(b.File)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	tmp := p + ".download"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- our backup dir
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("download the backup: %w", err)
	}
	return func() { _ = os.Remove(p) }, nil
}

// SetDestination changes where future backups of a database are uploaded.
func (s *Service) SetDestination(ctx context.Context, id int64, dest sql.NullInt64) error {
	return s.st.Q.SetDatabaseDestination(ctx, db.SetDatabaseDestinationParams{BackupDestinationID: dest, UpdatedAt: s.now().Unix(), ID: id})
}
