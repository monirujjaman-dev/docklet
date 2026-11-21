package server

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/databases"
	"github.com/monirujjaman-dev/docklet/internal/docker"
	"github.com/monirujjaman-dev/docklet/internal/host"
	"github.com/monirujjaman-dev/docklet/internal/release"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

type fileRoot struct {
	ID    string `json:"id"` // host, volume:<name>
	Label string `json:"label"`
	Path  string `json:"path"` // where it lives on the host
}

type volumeDocker interface {
	VolumeList(ctx context.Context) ([]docker.Volume, error)
	VolumeInspect(ctx context.Context, name string) (docker.Volume, error)
}

// POST /api/auth/reauth {password} — re-enter the password for sensitive
// actions (terminal, file changes) for the next 10 minutes.
func (s *Server) handleReauth(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.Password == "" {
		return errf(http.StatusBadRequest, "enter your password")
	}
	if err := s.requireRecentAuth(authFrom(r.Context()), in.Password); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "validForSec": int(recentAuth.Seconds())})
	return nil
}

func (s *Server) volumeLabel(ctx context.Context, v docker.Volume) string {
	if id, err := strconv.ParseInt(v.Labels[databases.LabelDatabase], 10, 64); err == nil {
		if d, err := s.st.Q.GetDatabase(ctx, id); err == nil {
			return "Database " + d.Name + " (" + v.Name + ")"
		}
	}
	switch v.Name {
	case "docklet-data", "docklet_docklet-data":
		return "Docklet data (" + v.Name + ")"
	}
	return "Volume " + v.Name
}

// GET /api/files/roots
func (s *Server) handleFileRoots(w http.ResponseWriter, r *http.Request) error {
	roots := []fileRoot{{ID: "host", Label: "Server", Path: "/"}}
	if vd, ok := s.docker.(volumeDocker); ok {
		vols, err := vd.VolumeList(r.Context())
		if err != nil {
			return err
		}
		for _, v := range vols {
			if v.Driver != "local" {
				continue
			}
			roots = append(roots, fileRoot{ID: "volume:" + v.Name, Label: s.volumeLabel(r.Context(), v), Path: v.Mountpoint})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": roots})
	return nil
}

// files returns the file manager for a root id.
func (s *Server) files(ctx context.Context, root string) (*host.Files, error) {
	h, err := s.hostService(ctx)
	if err != nil {
		return nil, err
	}
	var content host.Content = host.LocalContent{}
	if release.InContainer() {
		dk, ok := s.docker.(*docker.Client)
		if !ok {
			return nil, errf(http.StatusServiceUnavailable, "the file manager needs Docker")
		}
		img, err := host.SelfImage(ctx, dk.ContainerInspect)
		if err != nil {
			return nil, errf(http.StatusServiceUnavailable, "file manager unavailable: %v", err)
		}
		content = &host.DockerContent{DK: dk, Image: img}
	} else if hd, ok := h.R.(*host.Helper); ok { // tests: helper runner outside a container
		if dk, ok := hd.DK.(*docker.Client); ok {
			content = &host.DockerContent{DK: dk, Image: hd.Image}
		}
	}
	if s.filesContent != nil {
		content = s.filesContent
	}
	f := &host.Files{S: h, C: content, Root: "/"}
	if root == "" || root == "host" {
		return f, nil
	}
	name, ok := strings.CutPrefix(root, "volume:")
	if !ok || name == "" {
		return nil, errf(http.StatusBadRequest, "unknown root")
	}
	vd, ok := s.docker.(volumeDocker)
	if !ok {
		return nil, errf(http.StatusServiceUnavailable, "volumes need Docker")
	}
	v, err := vd.VolumeInspect(ctx, name)
	if err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return nil, errf(http.StatusNotFound, "volume %s not found", name)
		}
		return nil, err
	}
	if v.Mountpoint == "" || !strings.HasPrefix(v.Mountpoint, "/") {
		return nil, errf(http.StatusBadRequest, "volume %s has no local mountpoint", name)
	}
	f.Root = path.Clean(v.Mountpoint)
	return f, nil
}

func fileErr(err error) error {
	if errors.Is(err, host.ErrBinary) {
		return errf(http.StatusUnprocessableEntity, "%v", err)
	}
	return hostErr(err)
}

func (s *Server) auditFiles(r *http.Request, action, root string, details map[string]any) error {
	ctx := r.Context()
	if details == nil {
		details = map[string]any{}
	}
	details["root"] = root
	return s.st.Tx(ctx, func(q *db.Queries) error { return s.audit(ctx, q, r, action, "files", details) })
}

// requireFreshAuth guards file changes.
func (s *Server) requireFreshAuth(r *http.Request) error {
	return s.requireRecentAuth(authFrom(r.Context()), "")
}

// GET /api/files/list?root=&path=
func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := s.files(r.Context(), q.Get("root"))
	if err != nil {
		return err
	}
	l, err := f.List(r.Context(), q.Get("path"))
	if err != nil {
		return fileErr(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"listing": l, "warning": host.SystemWarning(path.Clean(f.Root + l.Path))})
	return nil
}

// GET /api/files/read?root=&path=
func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := s.files(r.Context(), q.Get("root"))
	if err != nil {
		return err
	}
	t, err := f.ReadText(r.Context(), q.Get("path"))
	if err != nil {
		return fileErr(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": t, "warning": host.SystemWarning(path.Clean(f.Root + t.Path))})
	return nil
}

// PUT /api/files/write {root, path, text, sha256}
func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Root   string `json:"root"`
		Path   string `json:"path"`
		Text   string `json:"text"`
		SHA256 string `json:"sha256"`
	}
	// JSON escaping can double the size of a 1 MB file.
	if err := readJSONMax(r, &in, host.MaxEdit*6+4096); err != nil {
		return err
	}
	if err := s.requireFreshAuth(r); err != nil {
		return err
	}
	f, err := s.files(r.Context(), in.Root)
	if err != nil {
		return err
	}
	sum, err := f.WriteText(r.Context(), in.Path, in.Text, in.SHA256)
	if err != nil {
		return fileErr(err)
	}
	if err := s.auditFiles(r, audit.ActionFileWritten, in.Root, map[string]any{"path": in.Path, "fromSha256": in.SHA256, "toSha256": sum, "bytes": len(in.Text)}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"sha256": sum})
	return nil
}

// POST /api/files/upload?root=&dir=&name=&overwrite=1 — raw body.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireFreshAuth(r); err != nil {
		return err
	}
	q := r.URL.Query()
	if r.ContentLength < 0 {
		return errf(http.StatusLengthRequired, "upload needs a Content-Length")
	}
	if r.ContentLength > host.MaxUpload {
		return errf(http.StatusRequestEntityTooLarge, "uploads are limited to 1 GB")
	}
	// A 1 GB upload takes longer than the server's read timeout.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(2 * time.Hour))
	f, err := s.files(r.Context(), q.Get("root"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	sum, err := f.Upload(ctx, q.Get("dir"), q.Get("name"), r.Body, r.ContentLength, q.Get("overwrite") == "1")
	if err != nil {
		return fileErr(err)
	}
	p := path.Join("/", q.Get("dir"), q.Get("name"))
	if err := s.auditFiles(r, audit.ActionFileUploaded, q.Get("root"), map[string]any{"path": p, "bytes": r.ContentLength, "sha256": sum}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p, "sha256": sum, "bytes": r.ContentLength})
	return nil
}

// GET /api/files/download?root=&path= — a file as itself, a folder as .tar.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	f, err := s.files(r.Context(), q.Get("root"))
	if err != nil {
		return err
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Hour))
	st, err := f.Stat(r.Context(), q.Get("path"))
	if err != nil {
		return fileErr(err)
	}
	if st.Type != "file" && st.Type != "dir" {
		return errf(http.StatusBadRequest, "only files and folders can be downloaded")
	}
	if err := s.auditFiles(r, audit.ActionFileDownloaded, q.Get("root"), map[string]any{"path": st.Path, "type": st.Type}); err != nil {
		return err
	}
	name := path.Base(st.Path)
	if name == "/" || name == "." {
		name = "root"
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		_, err := f.Download(ctx, q.Get("path"), pw)
		_ = pw.CloseWithError(err)
	}()
	defer func() { _ = pr.Close() }()
	if st.Type == "dir" {
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".tar"}))
		_, err := io.Copy(w, pr)
		return downloadErr(err)
	}
	tr := tar.NewReader(pr)
	for {
		h, err := tr.Next()
		if err != nil {
			return fileErr(fmt.Errorf("download: %w", err))
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(h.Size, 10))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		_, err = io.Copy(w, tr) //nolint:gosec // the size comes from the file on the host
		return downloadErr(err)
	}
}

// downloadErr hides errors once the body has started.
func downloadErr(err error) error {
	if err != nil {
		return errAlreadyWritten
	}
	return nil
}

// POST /api/files/{op} — mkdir, rename, move, delete, chmod, chown.
func (s *Server) handleFileOp(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Root      string `json:"root"`
		Path      string `json:"path"`
		Name      string `json:"name"`
		Dest      string `json:"dest"`
		Mode      string `json:"mode"`
		Owner     string `json:"owner"`
		Group     string `json:"group"`
		Recursive bool   `json:"recursive"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if err := s.requireFreshAuth(r); err != nil {
		return err
	}
	f, err := s.files(r.Context(), in.Root)
	if err != nil {
		return err
	}
	ctx := r.Context()
	op := r.PathValue("op")
	var res string
	var action string
	details := map[string]any{"path": in.Path}
	switch op {
	case "mkdir":
		res, err = f.Mkdir(ctx, in.Path, in.Name)
		action, details["path"] = audit.ActionFileMkdir, path.Join("/", in.Path, in.Name)
	case "rename":
		res, err = f.Rename(ctx, in.Path, in.Name)
		action, details["to"] = audit.ActionFileRenamed, in.Name
	case "move":
		res, err = f.Move(ctx, in.Path, in.Dest)
		action, details["to"] = audit.ActionFileMoved, in.Dest
	case "delete":
		res, err = f.Delete(ctx, in.Path)
		action = audit.ActionFileDeleted
	case "chmod":
		res, err = f.Chmod(ctx, in.Path, in.Mode, in.Recursive)
		action, details["mode"], details["recursive"] = audit.ActionFileChmod, in.Mode, in.Recursive
	case "chown":
		res, err = f.Chown(ctx, in.Path, in.Owner, in.Group, in.Recursive)
		action, details["owner"], details["group"], details["recursive"] = audit.ActionFileChown, in.Owner, in.Group, in.Recursive
	default:
		return errf(http.StatusNotFound, "unknown operation")
	}
	if err != nil {
		return fileErr(err)
	}
	if err := s.auditFiles(r, action, in.Root, details); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": res})
	return nil
}
