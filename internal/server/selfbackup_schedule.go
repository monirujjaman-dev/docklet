package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

// Scheduled, encrypted backups of Docklet itself to an S3 destination. The
// passphrase is stored under the master key so the schedule can run
// unattended; the admin must keep a copy, since the master key itself is in
// the backup.
const (
	sbDest       = "self_backup.destination"
	sbSchedule   = "self_backup.schedule"
	sbKeep       = "self_backup.keep"
	sbPassphrase = "self_backup.passphrase"
	sbIncludeDBs = "self_backup.include_db"
	sbLastAt     = "self_backup.last_at"
	sbLastError  = "self_backup.last_error"
	sbLastKey    = "self_backup.last_key"

	selfBackupPrefix = "docklet/"
)

var selfBackupIntervals = map[string]time.Duration{"off": 0, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour}

var sbPassphraseAAD = []byte("self-backup:passphrase")

type selfBackupRunner struct {
	mu      sync.Mutex
	running bool
}

func (s *Server) setting(ctx context.Context, key string) string {
	v, err := s.st.Q.GetSetting(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

type selfBackupPolicy struct {
	DestinationID          *int64 `json:"destinationId"`
	Schedule               string `json:"schedule"`
	Keep                   int64  `json:"keep"`
	IncludeDatabaseBackups bool   `json:"includeDatabaseBackups"`
	HasPassphrase          bool   `json:"hasPassphrase"`
	LastAt                 int64  `json:"lastAt,omitempty"`
	LastError              string `json:"lastError,omitempty"`
	LastKey                string `json:"lastKey,omitempty"`
	Running                bool   `json:"running"`
}

func (s *Server) selfBackupPolicy(ctx context.Context) selfBackupPolicy {
	p := selfBackupPolicy{Schedule: s.setting(ctx, sbSchedule), Keep: 7}
	if p.Schedule == "" {
		p.Schedule = "off"
	}
	if id, err := strconv.ParseInt(s.setting(ctx, sbDest), 10, 64); err == nil {
		p.DestinationID = &id
	}
	if k, err := strconv.ParseInt(s.setting(ctx, sbKeep), 10, 64); err == nil {
		p.Keep = k
	}
	p.IncludeDatabaseBackups = s.setting(ctx, sbIncludeDBs) == "1"
	p.HasPassphrase = s.setting(ctx, sbPassphrase) != ""
	p.LastAt, _ = strconv.ParseInt(s.setting(ctx, sbLastAt), 10, 64)
	p.LastError = s.setting(ctx, sbLastError)
	p.LastKey = s.setting(ctx, sbLastKey)
	s.sbRunner.mu.Lock()
	p.Running = s.sbRunner.running
	s.sbRunner.mu.Unlock()
	return p
}

// GET /api/system/backup-schedule
func (s *Server) handleGetSelfBackupSchedule(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, s.selfBackupPolicy(r.Context()))
	return nil
}

// PUT /api/system/backup-schedule {destinationId, schedule, keep, includeDatabaseBackups, passphrase?, password}
func (s *Server) handlePutSelfBackupSchedule(w http.ResponseWriter, r *http.Request) error {
	if s.cfg.DataDir == "" {
		return errf(http.StatusServiceUnavailable, "backups are not available in this configuration")
	}
	var in struct {
		DestinationID          *int64 `json:"destinationId"`
		Schedule               string `json:"schedule"`
		Keep                   int64  `json:"keep"`
		IncludeDatabaseBackups bool   `json:"includeDatabaseBackups"`
		Passphrase             string `json:"passphrase"`
		Password               string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	if err := checkPassword(a.User, in.Password); err != nil {
		return err
	}
	if _, ok := selfBackupIntervals[in.Schedule]; !ok {
		return errf(http.StatusBadRequest, "schedule must be off, daily or weekly")
	}
	if in.Keep < 1 || in.Keep > 365 {
		return errf(http.StatusBadRequest, "keep must be 1-365")
	}
	cur := s.selfBackupPolicy(ctx)
	if in.Schedule != "off" {
		if in.DestinationID == nil {
			return errf(http.StatusBadRequest, "choose a destination")
		}
		if _, err := s.st.Q.GetDestination(ctx, *in.DestinationID); err != nil {
			return errf(http.StatusBadRequest, "unknown destination")
		}
		if in.Passphrase == "" && !cur.HasPassphrase {
			return errf(http.StatusBadRequest, "set an encryption passphrase")
		}
	}
	if in.Passphrase != "" && len(in.Passphrase) < minBackupPassphrase {
		return errf(http.StatusBadRequest, "the encryption passphrase must be at least %d characters", minBackupPassphrase)
	}
	now := s.now()
	err := s.st.Tx(ctx, func(q *db.Queries) error {
		dest := ""
		if in.DestinationID != nil {
			dest = strconv.FormatInt(*in.DestinationID, 10)
		}
		inc := "0"
		if in.IncludeDatabaseBackups {
			inc = "1"
		}
		for k, v := range map[string]string{sbDest: dest, sbSchedule: in.Schedule, sbKeep: strconv.FormatInt(in.Keep, 10), sbIncludeDBs: inc} {
			if err := putSetting(ctx, q, k, v, now); err != nil {
				return err
			}
		}
		if in.Passphrase != "" {
			enc, err := s.box.Seal([]byte(in.Passphrase), sbPassphraseAAD)
			if err != nil {
				return err
			}
			if err := putSetting(ctx, q, sbPassphrase, base64.StdEncoding.EncodeToString(enc), now); err != nil {
				return err
			}
		}
		return s.audit(ctx, q, r, audit.ActionSelfBackupPolicy, "system", map[string]any{
			"schedule": in.Schedule, "destination": dest, "keep": in.Keep, "includeDatabaseBackups": in.IncludeDatabaseBackups,
			"passphraseChanged": in.Passphrase != "",
		})
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.selfBackupPolicy(ctx))
	return nil
}

// POST /api/system/backup-schedule/run — back up to the destination now.
func (s *Server) handleRunSelfBackup(w http.ResponseWriter, r *http.Request) error {
	p := s.selfBackupPolicy(r.Context())
	if p.DestinationID == nil || !p.HasPassphrase {
		return errf(http.StatusBadRequest, "configure a destination and passphrase first")
	}
	if err := s.st.Tx(r.Context(), func(q *db.Queries) error {
		return s.audit(r.Context(), q, r, audit.ActionSystemBackup, "system", map[string]any{"to": "destination"})
	}); err != nil {
		return err
	}
	go s.runSelfBackup(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
	return nil
}

// runSelfBackupSchedule checks every interval whether a backup is due.
func (s *Server) runSelfBackupSchedule(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p := s.selfBackupPolicy(ctx)
		iv := selfBackupIntervals[p.Schedule]
		if iv == 0 || p.DestinationID == nil || !p.HasPassphrase {
			continue
		}
		if s.now().Sub(time.Unix(p.LastAt, 0)) >= iv {
			s.runSelfBackup(ctx)
		}
	}
}

// runSelfBackup writes an encrypted backup to a temporary file, uploads it
// and applies retention. Only one runs at a time.
func (s *Server) runSelfBackup(ctx context.Context) {
	s.sbRunner.mu.Lock()
	if s.sbRunner.running {
		s.sbRunner.mu.Unlock()
		return
	}
	s.sbRunner.running = true
	s.sbRunner.mu.Unlock()
	defer func() {
		s.sbRunner.mu.Lock()
		s.sbRunner.running = false
		s.sbRunner.mu.Unlock()
	}()

	p := s.selfBackupPolicy(ctx)
	key, err := s.selfBackupOnce(ctx, p)
	now := s.now()
	msg := ""
	action, details := audit.ActionSelfBackupUploaded, map[string]any{"key": key}
	if err != nil {
		msg = err.Error()
		action, details["error"] = audit.ActionSelfBackupFailed, msg
		s.log.Warn("scheduled Docklet backup failed", "err", err)
	}
	_ = s.st.Tx(context.WithoutCancel(ctx), func(q *db.Queries) error {
		_ = putSetting(ctx, q, sbLastAt, strconv.FormatInt(now.Unix(), 10), now)
		_ = putSetting(ctx, q, sbLastError, msg, now)
		if err == nil {
			_ = putSetting(ctx, q, sbLastKey, key, now)
		}
		_, aerr := audit.Append(ctx, q, audit.Entry{Actor: "system", Action: action, Target: "system", Details: details}, now)
		return aerr
	})
}

func (s *Server) selfBackupOnce(ctx context.Context, p selfBackupPolicy) (string, error) {
	if p.DestinationID == nil {
		return "", errors.New("no destination configured")
	}
	raw, err := base64.StdEncoding.DecodeString(s.setting(ctx, sbPassphrase))
	if err != nil {
		return "", errors.New("no passphrase configured")
	}
	pass, err := s.box.Open(raw, sbPassphraseAAD)
	if err != nil {
		return "", fmt.Errorf("decrypt the backup passphrase: %w", err)
	}
	c, dest, err := s.dests.Client(ctx, *p.DestinationID)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(s.cfg.DataDir, ".selfbackup-upload-")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	opt := selfbackup.Options{Passphrase: string(pass)}
	if p.IncludeDatabaseBackups {
		opt.BackupDir = s.cfg.BackupDir
	}
	if _, err := selfbackup.Write(ctx, f, s.st, s.cfg.DataDir, opt); err != nil {
		return "", err
	}
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	key := path.Join(selfBackupPrefix, fmt.Sprintf("docklet-%s-%s.dkbackup", sanitizeFilename(host), s.now().UTC().Format("20060102-150405")))
	uctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	if err := c.Upload(uctx, key, f, fi.Size()); err != nil {
		return key, fmt.Errorf("upload to %s: %w", dest.Name, err)
	}
	// Retention: the newest Keep archives under docklet/ (names sort by time).
	objs, err := c.List(ctx, selfBackupPrefix)
	if err != nil {
		return key, nil
	}
	var names []string
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".dkbackup") {
			names = append(names, o.Key)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, n := range names {
		if int64(i) >= p.Keep {
			_ = c.Delete(ctx, n)
		}
	}
	return key, nil
}

// GET /api/system/remote-backups — Docklet backups in the destination.
func (s *Server) handleListRemoteSelfBackups(w http.ResponseWriter, r *http.Request) error {
	p := s.selfBackupPolicy(r.Context())
	if p.DestinationID == nil {
		writeJSON(w, http.StatusOK, map[string]any{"backups": []any{}})
		return nil
	}
	c, _, err := s.dests.Client(r.Context(), *p.DestinationID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	objs, err := c.List(ctx, selfBackupPrefix)
	if err != nil {
		return errf(http.StatusBadGateway, "%v", err)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key > objs[j].Key })
	writeJSON(w, http.StatusOK, map[string]any{"backups": objs})
	return nil
}

var _ = sql.ErrNoRows
