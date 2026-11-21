package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
	"github.com/monirujjaman-dev/docklet/internal/version"
)

const minBackupPassphrase = 12

// POST /api/system/backup {password, passphrase, includeDatabaseBackups}
// streams an encrypted self-backup. It contains the master key, so it needs
// the account password again and is always encrypted.
func (s *Server) handleSystemBackup(w http.ResponseWriter, r *http.Request) error {
	if s.cfg.DataDir == "" {
		return errf(http.StatusServiceUnavailable, "backups are not available in this configuration")
	}
	var in struct {
		Password               string `json:"password"`
		Passphrase             string `json:"passphrase"`
		IncludeDatabaseBackups bool   `json:"includeDatabaseBackups"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	a := authFrom(ctx)
	if err := checkPassword(a.User, in.Password); err != nil {
		return err
	}
	if len(in.Passphrase) < minBackupPassphrase {
		return errf(http.StatusBadRequest, "the encryption passphrase must be at least %d characters", minBackupPassphrase)
	}
	opt := selfbackup.Options{Passphrase: in.Passphrase}
	if in.IncludeDatabaseBackups {
		opt.BackupDir = s.cfg.BackupDir
	}
	if err := s.st.Tx(ctx, func(q *db.Queries) error {
		_, err := audit.Append(ctx, q, audit.Entry{
			ActorID: ptr(a.User.ID), Actor: a.User.Username, Action: audit.ActionSystemBackup, IP: clientIP(r),
			Details: map[string]any{"includeDatabaseBackups": in.IncludeDatabaseBackups},
		}, s.now())
		return err
	}); err != nil {
		return err
	}
	host, _ := os.Hostname()
	name := fmt.Sprintf("docklet-%s-%s.dkbackup", sanitizeFilename(host), s.now().UTC().Format("20060102-150405"))
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if _, err := selfbackup.Write(ctx, w, s.st, s.cfg.DataDir, opt); err != nil {
		// Headers are sent; abort the connection so the download fails
		// visibly instead of leaving a truncated file that looks complete.
		s.log.Error("self-backup failed", "err", err)
		panic(http.ErrAbortHandler)
	}
	return nil
}

func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, s)
	if s == "" || len(s) > 40 {
		return "server"
	}
	return s
}

type updateInfo struct {
	Current     string    `json:"current"`
	Latest      string    `json:"latest,omitempty"`
	URL         string    `json:"url,omitempty"`
	PublishedAt time.Time `json:"publishedAt,omitzero"`
	Newer       bool      `json:"newer"`
	Comparable  bool      `json:"comparable"`
	InContainer bool      `json:"inContainer"`
	CheckedAt   int64     `json:"checkedAt"`
	Error       string    `json:"error,omitempty"`
}

type updateChecker struct {
	mu   sync.Mutex
	last updateInfo
	at   time.Time
	rel  *release.Client
}

// GET /api/system/update[?refresh=1] — asks GitHub for the latest release,
// only when the admin opens this check (cached for an hour).
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) error {
	if s.cfg.DisableUpdateCheck {
		writeJSON(w, http.StatusOK, map[string]any{"current": version.Version, "disabled": true})
		return nil
	}
	u := s.updates
	u.mu.Lock()
	defer u.mu.Unlock()
	if r.URL.Query().Get("refresh") == "" && !u.at.IsZero() && time.Since(u.at) < time.Hour {
		writeJSON(w, http.StatusOK, u.last)
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	info := updateInfo{Current: version.Version, InContainer: release.InContainer(), CheckedAt: s.now().Unix()}
	rel, err := u.rel.Latest(ctx, "")
	if err != nil {
		info.Error = err.Error()
	} else {
		info.Latest, info.URL, info.PublishedAt = rel.Tag, rel.URL, rel.PublishedAt
		cmp, ok := release.Compare(version.Version, rel.Tag)
		info.Comparable, info.Newer = ok, ok && cmp < 0
	}
	u.last, u.at = info, time.Now()
	writeJSON(w, http.StatusOK, info)
	return nil
}
