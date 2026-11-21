package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.0.0", "v1.0.1", -1, true}, {"1.2.0", "v1.10.0", -1, true}, {"v2.0.0", "v1.9.9", 1, true},
		{"v1.0.0", "1.0.0", 0, true}, {"main", "v1.0.0", 0, false}, {"v1.0.0-3-gabc", "v1.0.0", 0, false},
	} {
		if cmp, ok := Compare(c.a, c.b); cmp != c.cmp || ok != c.ok {
			t.Errorf("Compare(%s, %s) = %d, %v", c.a, c.b, cmp, ok)
		}
	}
}

// fakeGitHub serves a release whose binary is a shell script.
func fakeGitHub(t *testing.T, binary []byte, sum string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v1.2.0","html_url":"https://x","assets":[
			{"name":%q,"browser_download_url":"%s/bin"},
			{"name":"checksums.txt","browser_download_url":"%s/sums"}]}`, AssetName(), srv.URL, srv.URL)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  docklet-other-arch\n%s  %s\n", strings.Repeat("0", 64), sum, AssetName())
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New("o/r")
	c.APIBase = srv.URL
	return c
}

func TestInstall(t *testing.T) {
	bin := []byte("#!/bin/sh\necho 'docklet v1.2.0'\n")
	s := sha256.Sum256(bin)
	c := fakeGitHub(t, bin, hex.EncodeToString(s[:]))
	exe := filepath.Join(t.TempDir(), "docklet")
	_ = os.WriteFile(exe, []byte("old binary"), 0o755) //nolint:gosec // test
	r, err := c.Latest(context.Background(), "")
	if err != nil || r.Tag != "v1.2.0" {
		t.Fatalf("latest = %+v, %v", r, err)
	}
	out, err := c.Install(context.Background(), r, exe)
	if err != nil {
		t.Fatal(err)
	}
	if out != "docklet v1.2.0" {
		t.Fatalf("version output = %q", out)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(bin) { //nolint:gosec // test paths
		t.Fatal("binary not replaced")
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != "old binary" { //nolint:gosec // test paths
		t.Fatal("previous binary not kept")
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	c := fakeGitHub(t, []byte("#!/bin/sh\necho evil\n"), strings.Repeat("a", 64))
	exe := filepath.Join(t.TempDir(), "docklet")
	_ = os.WriteFile(exe, []byte("old binary"), 0o755) //nolint:gosec // test
	r, _ := c.Latest(context.Background(), "")
	if _, err := c.Install(context.Background(), r, exe); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" { //nolint:gosec // test paths
		t.Fatal("binary replaced despite a bad checksum")
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}
