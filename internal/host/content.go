package host

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// ArchiveDocker is what DockerContent needs.
type ArchiveDocker interface {
	ContainerCreate(ctx context.Context, s docker.ContainerSpec) (string, error)
	ContainerRemove(ctx context.Context, id string) error
	CopyToOwned(ctx context.Context, id, dir string, hdr *tar.Header, content io.Reader, keepOwner bool) error
	CopyArchive(ctx context.Context, id, path string, w io.Writer) error
}

// DockerContent moves file contents through Docker's archive API: a helper
// container (created, never started) bind-mounts the host directory, and
// dockerd reads or writes it. Nothing runs inside the container.
type DockerContent struct {
	DK    ArchiveDocker
	Image string
}

func (d *DockerContent) helper(ctx context.Context, hostDir string) (string, func(), error) {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	id, err := d.DK.ContainerCreate(ctx, docker.ContainerSpec{
		Name: "docklet-files-" + hex.EncodeToString(rnd[:]), Image: d.Image, Entrypoint: []string{"true"},
		Binds: []string{hostDir + ":/x"}, Network: "none", RestartPolicy: "no",
		Labels: map[string]string{"docklet.managed": "true", LabelHelper: "true"},
	})
	if err != nil {
		return "", nil, fmt.Errorf("file helper: %w", err)
	}
	return id, func() { _ = d.DK.ContainerRemove(context.WithoutCancel(ctx), id) }, nil
}

// Put implements Content.
func (d *DockerContent) Put(ctx context.Context, dir, name string, r io.Reader, size, mode int64, uid, gid int) error {
	id, done, err := d.helper(ctx, dir)
	if err != nil {
		return err
	}
	defer done()
	hdr := &tar.Header{Name: name, Mode: mode, Size: size, Typeflag: tar.TypeReg, Uid: uid, Gid: gid, ModTime: time.Now()}
	return d.DK.CopyToOwned(ctx, id, "/x", hdr, io.LimitReader(r, size), true)
}

// Get implements Content.
func (d *DockerContent) Get(ctx context.Context, p string, w io.Writer) error {
	dir, base := splitDir(p)
	id, done, err := d.helper(ctx, dir)
	if err != nil {
		return err
	}
	defer done()
	src := "/x/" + base
	if base == "" {
		src = "/x/."
	}
	return d.DK.CopyArchive(ctx, id, src, w)
}

func splitDir(p string) (string, string) {
	if p == "/" {
		return "/", ""
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/", p[1:]
			}
			return p[:i], p[i+1:]
		}
	}
	return ".", p
}

// LocalContent reads and writes host files directly (Docklet runs on the
// host as root).
type LocalContent struct{}

// Put implements Content.
func (LocalContent) Put(_ context.Context, dir, name string, r io.Reader, size, mode int64, uid, gid int) error {
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(mode&0o7777)) //nolint:gosec // path resolved and checked by Files
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("upload ended early (%d of %d bytes)", n, size)
	}
	if err == nil {
		err = os.Chown(p, uid, gid)
	}
	if err == nil {
		err = os.Chmod(p, os.FileMode(mode&0o7777)) //nolint:gosec // mode copied from the file being replaced
	}
	if err != nil {
		_ = os.Remove(p)
	}
	return err
}

// Get implements Content.
func (LocalContent) Get(_ context.Context, p string, w io.Writer) error {
	tw := tar.NewWriter(w)
	base := filepath.Dir(p)
	err := filepath.Walk(p, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(path) //nolint:gosec // inside the checked path
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
