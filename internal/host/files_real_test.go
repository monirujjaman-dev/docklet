package host

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// TestRealFiles exercises the file manager on the machine running the tests
// (the Docker host, as root), rooted at a Docker volume:
//
//	DOCKLET_TEST_FILES=1 DOCKLET_HOST_PID=$(pgrep -x dockerd) go test ./internal/host/ -run TestRealFiles -v
func TestRealFiles(t *testing.T) {
	if os.Getenv("DOCKLET_TEST_FILES") == "" {
		t.Skip("set DOCKLET_TEST_FILES=1 and DOCKLET_HOST_PID (see the comment)")
	}
	ctx := context.Background()
	dk := docker.New("/var/run/docker.sock")
	pid := int(atoi64(os.Getenv("DOCKLET_HOST_PID")))
	if pid == 0 {
		pid = 1
	}
	svc := &Service{R: &Helper{DK: dk, Image: "alpine:3.20", TargetPID: pid}}
	sh := func(c string) string {
		out, _ := exec.Command("sh", "-c", c).CombinedOutput() //nolint:gosec // test
		return strings.TrimSpace(string(out))
	}
	sh("docker volume rm -f files-test; docker volume create files-test; mkdir -p /tmp/files-outside; echo secret > /tmp/files-outside/secret")
	t.Cleanup(func() { sh("docker volume rm -f files-test") })
	mp := sh("docker volume inspect -f '{{.Mountpoint}}' files-test")
	content := &DockerContent{DK: dk, Image: "alpine:3.20"}
	f := &Files{S: svc, C: content, Root: mp}

	// Escape attempts: ../ and symlinks pointing outside.
	sh("mkdir -p " + mp + "/app && ln -s /tmp/files-outside " + mp + "/app/out && ln -s ../../../../../../etc/hostname " + mp + "/app/host")
	for _, p := range []string{"/../../../etc", "/app/out", "/app/out/secret", "/app/host"} {
		if _, err := f.List(ctx, p); err == nil && p != "/../../../etc" {
			t.Errorf("list %s escaped", p)
		}
		if _, err := f.ReadText(ctx, p); err == nil {
			t.Errorf("read %s escaped", p)
		}
		var sink strings.Builder
		if _, err := f.Download(ctx, p, &sink); err == nil && p != "/../../../etc" {
			t.Errorf("download %s escaped", p)
		}
	}
	if l, err := f.List(ctx, "/../../../"); err != nil || l.Path != "/" {
		t.Fatalf("../ not clamped to the root: %+v %v", l, err)
	}
	if _, err := f.WriteText(ctx, "/app/out/secret", "pwned", ""); err == nil {
		t.Fatal("write through a symlink escaped")
	}
	if _, err := f.Upload(ctx, "/app/out", "x", strings.NewReader("x"), 1, false); err == nil {
		t.Fatal("upload through a symlink escaped")
	}
	if got := sh("cat /tmp/files-outside/secret; ls /tmp/files-outside"); got != "secret\nsecret" {
		t.Fatalf("outside dir changed: %q", got)
	}
	// Deleting the symlink removes the link, not the target.
	if _, err := f.Delete(ctx, "/app/out"); err != nil {
		t.Fatal(err)
	}
	if sh("cat /tmp/files-outside/secret") != "secret" {
		t.Fatal("delete followed the symlink")
	}

	// Text edit with conflict check, keeping mode and owner.
	sh("printf 'hello\\n' > " + mp + "/app/conf.txt; chown 999:999 " + mp + "/app/conf.txt; chmod 640 " + mp + "/app/conf.txt")
	tf, err := f.ReadText(ctx, "/app/conf.txt")
	if err != nil || tf.Text != "hello\n" || tf.Mode != "0640" {
		t.Fatalf("read = %+v %v", tf, err)
	}
	if _, err := f.WriteText(ctx, "/app/conf.txt", "x", "0000"); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("stale write: %v", err)
	}
	if _, err := f.WriteText(ctx, "/app/conf.txt", "hello world\n", tf.SHA256); err != nil {
		t.Fatal(err)
	}
	if got := sh("cat " + mp + "/app/conf.txt; stat -c '%a %u %g' " + mp + "/app/conf.txt; ls -a " + mp + "/app | grep -c docklet"); got != "hello world\n640 999 999\n0" {
		t.Fatalf("after write: %q", got)
	}
	sh("head -c 2000 /dev/urandom > " + mp + "/app/bin")
	if _, err := f.ReadText(ctx, "/app/bin"); !errors.Is(err, ErrBinary) {
		t.Fatalf("binary: %v", err)
	}

	// Upload 50 MB and download it back.
	const size = 50 << 20
	h := sha256.New()
	pr, pw := io.Pipe()
	go func() {
		_, err := io.CopyN(io.MultiWriter(pw, h), rand.Reader, size)
		_ = pw.CloseWithError(err)
	}()
	sum, err := f.Upload(ctx, "/app", "big.bin", pr, size, false)
	want := hex.EncodeToString(h.Sum(nil))
	if err != nil || sum != want {
		t.Fatalf("upload: %s %v, want %s", sum, err, want)
	}
	if _, err := f.Upload(ctx, "/app", "big.bin", strings.NewReader("x"), 1, false); err == nil {
		t.Fatal("upload overwrote without asking")
	}
	dr, dw := io.Pipe()
	go func() {
		_, err := f.Download(ctx, "/app/big.bin", dw)
		_ = dw.CloseWithError(err)
	}()
	tr := tar.NewReader(dr)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	h2 := sha256.New()
	n, _ := io.Copy(h2, tr) //nolint:gosec // test archive
	_, _ = io.Copy(io.Discard, dr)
	if hdr.Name != "big.bin" || n != size || hex.EncodeToString(h2.Sum(nil)) != want {
		t.Fatalf("download %s %d bytes, sha mismatch", hdr.Name, n)
	}

	// Folder operations.
	if _, err := f.Mkdir(ctx, "/app", "sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Move(ctx, "/app/big.bin", "/app/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Move(ctx, "/app", "/app/sub"); err == nil {
		t.Fatal("moved a folder into itself")
	}
	if p, err := f.Rename(ctx, "/app/sub/big.bin", "renamed.bin"); err != nil || p != "/app/sub/renamed.bin" {
		t.Fatalf("rename: %s %v", p, err)
	}
	if _, err := f.Chmod(ctx, "/app/sub", "0700", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Chown(ctx, "/app/sub", "1000", "1000", true); err != nil {
		t.Fatal(err)
	}
	if got := sh("stat -c '%a %u' " + mp + "/app/sub/renamed.bin"); got != "700 1000" {
		t.Fatalf("chmod/chown: %q", got)
	}
	l, err := f.List(ctx, "/app")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Type+":"+e.Name)
	}
	if strings.Join(names, ",") != "dir:sub,file:bin,file:conf.txt,link:host" {
		t.Fatalf("list = %v", names)
	}
	if _, err := f.Delete(ctx, "/"); err == nil {
		t.Fatal("deleted the volume root")
	}
	if _, err := f.Delete(ctx, "/app/sub"); err != nil {
		t.Fatal(err)
	}

	// Host root: system folders are protected.
	hf := &Files{S: svc, C: content, Root: "/"}
	for _, p := range []string{"/etc", "/", "/usr/bin", "/proc/1"} {
		if _, err := hf.Delete(ctx, p); err == nil {
			t.Errorf("deleted %s", p)
		}
	}
	if l, err := hf.List(ctx, "/etc"); err != nil || len(l.Entries) < 10 {
		t.Fatalf("list /etc: %d %v", len(l.Entries), err)
	}
}
