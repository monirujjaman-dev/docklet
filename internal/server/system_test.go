package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/selfbackup"
)

func TestSystemBackupAndUpdateCheck(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "master.key"), bytes.Repeat([]byte{3}, 32), 0o600)
	calls := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, `{"tag_name":"v9.9.9","html_url":"https://github.com/x/releases/v9.9.9","published_at":"2026-11-20T00:00:00Z","assets":[]}`)
	}))
	defer gh.Close()
	env := startPanelServer(t, filepath.Join(dir, "docklet.db"), true,
		func(s *Server) { s.cfg.DataDir = dir; s.cfg.BackupDir = filepath.Join(dir, "backups") },
		WithReleaseAPI(gh.URL))
	c := env.client(t)

	if code, _ := c.do("POST", "/api/system/backup", map[string]any{"password": "wrong", "passphrase": "long enough phrase"}); code < 400 {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := c.do("POST", "/api/system/backup", map[string]any{"password": adminPW, "passphrase": "short"}); code != 400 {
		t.Fatalf("short passphrase: %d", code)
	}
	req, _ := http.NewRequest("POST", c.base+"/api/system/backup", strings.NewReader(`{"password":"`+adminPW+`","passphrase":"long enough phrase"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, c.csrf)
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(res.Body)
	closeBody(t, res)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Disposition"), ".dkbackup") {
		t.Fatalf("download: %d %v %s", res.StatusCode, res.Header, data)
	}
	m, err := selfbackup.Inspect(bytes.NewReader(data), "long enough phrase")
	if err != nil || !m.Encrypted || m.Files["master.key"] == "" {
		t.Fatalf("archive: %+v %v", m, err)
	}
	if !strings.Contains(strings.Join(actionsOf(t, env.st), ","), "system.backup_downloaded") {
		t.Fatal("backup download not audited")
	}

	_, u := c.do("GET", "/api/system/update", nil)
	if u["latest"] != "v9.9.9" || u["current"] != "dev" || u["comparable"] != false {
		t.Fatalf("update = %v", u)
	}
	c.do("GET", "/api/system/update", nil)
	if calls != 1 {
		t.Fatalf("update check not cached: %d calls", calls)
	}
	c.do("GET", "/api/system/update?refresh=1", nil)
	if calls != 2 {
		t.Fatalf("refresh ignored: %d calls", calls)
	}
}
