package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The file manager works on a root: "/" for the host, or a Docker volume's
// mountpoint. Every path is resolved on the host (realpath, so symlinks are
// followed) and must stay inside the root. Metadata changes run as small
// scripts through the Runner; file contents stream through a Content
// implementation (the Docker archive API on a helper container), so large
// uploads and downloads never pass through a shell or an env var.

// MaxEdit is the largest file the editor opens.
const MaxEdit = 1 << 20

// MaxUpload is the largest upload.
const MaxUpload = 1 << 30

// Entry is one directory entry.
type Entry struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // dir, file, link, other
	Size   int64  `json:"size"`
	Mode   string `json:"mode"` // octal, e.g. 0644
	Owner  string `json:"owner"`
	Group  string `json:"group"`
	MTime  int64  `json:"mtime"`
	Target string `json:"target,omitempty"` // symlink target
}

// Listing is a directory's contents.
type Listing struct {
	Path      string  `json:"path"` // relative to the root, starts with /
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// Content moves file bytes in and out of host directories.
type Content interface {
	// Put writes a regular file name into the host directory dir.
	Put(ctx context.Context, dir, name string, r io.Reader, size int64, mode int64, uid, gid int) error
	// Get streams a host path as a tar archive.
	Get(ctx context.Context, p string, w io.Writer) error
}

// Files is the file manager for one root.
type Files struct {
	S    *Service
	C    Content
	Root string // absolute host path, "/" for the whole host
}

// Clean turns a user path into a root-relative path starting with "/".
func Clean(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", invalid("invalid path")
	}
	return path.Clean("/" + p), nil
}

func (f *Files) abs(rel string) (string, error) {
	rel, err := Clean(rel)
	if err != nil {
		return "", err
	}
	return path.Clean(f.Root + rel), nil
}

func (f *Files) rel(abs string) string {
	if f.Root == "/" {
		return abs
	}
	r := strings.TrimPrefix(abs, f.Root)
	if r == "" {
		return "/"
	}
	return r
}

// resolve is shared by the scripts: it sets $R to the real path of $P
// (following symlinks unless NOFOLLOW=1, which resolves only the parent) and
// exits 5 if it leaves $ROOT.
const resolve = `
inroot() { [ "$ROOT" = / ] && return 0; case "$1" in "$ROOT"|"$ROOT"/*) return 0;; esac; return 1; }
if [ "$NOFOLLOW" = 1 ]; then d=$(realpath -m -- "$(dirname -- "$P")") && R="$d/$(basename -- "$P")"; R=$(printf '%s' "$R" | sed 's#^//#/#')
else R=$(realpath -m -- "$P"); fi
inroot "$R" || { echo "path leaves the volume" >&2; exit 5; }
`

func (f *Files) run(ctx context.Context, script string, p string, env ...string) (string, error) {
	out, err := Script(ctx, f.S.R, "set -e"+resolve+script, append([]string{"ROOT=" + f.Root, "P=" + p}, env...)...)
	var ce *ErrCommand
	if errors.As(err, &ce) {
		msg := strings.TrimSpace(ce.Res.Stderr)
		if msg == "" {
			msg = ce.Error()
		}
		return out, invalid("%s", msg)
	}
	return out, err
}

const listScript = `[ -d "$R" ] || { echo "not a directory" >&2; exit 3; }
echo "$R"
find "$R" -mindepth 1 -maxdepth 1 -printf '%y\t%s\t%#m\t%u\t%g\t%T@\t%l\t%f\0' 2>/dev/null | head -z -n 5001 | tr '\0\n' '\n\001'`

// List lists a directory.
func (f *Files) List(ctx context.Context, rel string) (Listing, error) {
	p, err := f.abs(rel)
	if err != nil {
		return Listing{}, err
	}
	out, err := f.run(ctx, listScript, p)
	if err != nil {
		return Listing{}, err
	}
	real, rest, _ := strings.Cut(out, "\n")
	l := Listing{Path: f.rel(strings.TrimSpace(real)), Entries: []Entry{}}
	for _, line := range strings.Split(rest, "\n") {
		if line == "" {
			continue
		}
		line = strings.ReplaceAll(line, "\001", "\n") // names with newlines
		fs := strings.SplitN(line, "\t", 8)
		if len(fs) != 8 {
			continue
		}
		e := Entry{Size: atoi64(fs[1]), Mode: fs[2], Owner: fs[3], Group: fs[4], Target: fs[6], Name: fs[7]}
		if t, err := strconv.ParseFloat(fs[5], 64); err == nil {
			e.MTime = int64(t)
		}
		switch fs[0] {
		case "d":
			e.Type = "dir"
		case "f":
			e.Type = "file"
		case "l":
			e.Type = "link"
		default:
			e.Type = "other"
		}
		l.Entries = append(l.Entries, e)
	}
	if len(l.Entries) > 5000 {
		l.Entries, l.Truncated = l.Entries[:5000], true
	}
	sort.Slice(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return l, nil
}

// FileStat describes one path.
type FileStat struct {
	Path string `json:"path"`
	Real string `json:"-"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	Mode int64  `json:"mode"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
}

const statScript = `[ -e "$R" ] || [ -L "$R" ] || { echo "MISSING"; echo "$R"; exit 0; }
echo "$R"; stat -c '%F|%s|%a|%u|%g' -- "$R"`

// Stat stats a path (following symlinks), or reports it missing (Type "").
func (f *Files) Stat(ctx context.Context, rel string) (FileStat, error) {
	p, err := f.abs(rel)
	if err != nil {
		return FileStat{}, err
	}
	out, err := f.run(ctx, statScript, p)
	if err != nil {
		return FileStat{}, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] == "MISSING" {
		real := p
		if len(lines) > 1 {
			real = lines[1]
		}
		return FileStat{Path: f.rel(real), Real: real}, nil
	}
	if len(lines) < 2 {
		return FileStat{}, fmt.Errorf("stat: unexpected output %q", out)
	}
	parts := strings.Split(lines[1], "|")
	if len(parts) != 5 {
		return FileStat{}, fmt.Errorf("stat: unexpected output %q", out)
	}
	st := FileStat{Path: f.rel(lines[0]), Real: lines[0], Size: atoi64(parts[1]), UID: int(atoi64(parts[3])), GID: int(atoi64(parts[4]))}
	st.Mode, _ = strconv.ParseInt(parts[2], 8, 64)
	switch parts[0] {
	case "directory":
		st.Type = "dir"
	case "regular file", "regular empty file":
		st.Type = "file"
	default:
		st.Type = "other"
	}
	return st, nil
}

// TextFile is a file opened in the editor.
type TextFile struct {
	Path   string `json:"path"`
	Text   string `json:"text"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

// ErrBinary means the file isn't UTF-8 text.
var ErrBinary = errors.New("this file is not UTF-8 text; download it instead")

const readScript = `[ -f "$R" ] || { echo "not a regular file" >&2; exit 3; }
s=$(stat -c %s -- "$R"); [ "$s" -le ` + "1048576" + ` ] || { echo "file is larger than 1 MB; download it instead" >&2; exit 3; }
echo "$R"; stat -c %a -- "$R"; base64 -w0 -- "$R"; echo`

// ReadText opens a text file (≤ 1 MB, UTF-8 without NUL bytes).
func (f *Files) ReadText(ctx context.Context, rel string) (TextFile, error) {
	p, err := f.abs(rel)
	if err != nil {
		return TextFile{}, err
	}
	out, err := f.run(ctx, readScript, p)
	if err != nil {
		return TextFile{}, err
	}
	parts := strings.SplitN(out, "\n", 3)
	if len(parts) < 3 {
		return TextFile{}, fmt.Errorf("read: unexpected output")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[2]))
	if err != nil {
		return TextFile{}, err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return TextFile{}, ErrBinary
	}
	sum := sha256.Sum256(data)
	return TextFile{Path: f.rel(parts[0]), Text: string(data), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Mode: "0" + strings.TrimSpace(parts[1])}, nil
}

func tmpName(name string) string {
	return "." + name + ".docklet-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// placeScript moves the uploaded temp file over the target: if EXPECT is set
// the current file must still have that sha256 (nobody changed it since it
// was opened); NOCLOBBER refuses to replace an existing file.
const placeScript = `T="$(dirname -- "$R")/$TMP"
fail() { rm -f -- "$T"; echo "$1" >&2; exit 3; }
if [ -n "$EXPECT" ]; then
  [ -f "$R" ] || fail "the file was deleted since you opened it"
  [ "$(sha256sum -- "$R" | cut -d' ' -f1)" = "$EXPECT" ] || fail "the file changed since you opened it; reload it and try again"
fi
if [ "$NOCLOBBER" = 1 ] && { [ -e "$R" ] || [ -L "$R" ]; }; then fail "a file with that name already exists"; fi
[ -d "$R" ] && fail "a directory with that name exists"
mv -f -- "$T" "$R"
sha256sum -- "$R" | cut -d' ' -f1`

// WriteText saves text over rel. expectSHA is the sha256 the file had when
// opened ("" for a new file). Mode and owner of an existing file are kept.
func (f *Files) WriteText(ctx context.Context, rel, text, expectSHA string) (string, error) {
	if len(text) > MaxEdit {
		return "", invalid("text is larger than 1 MB")
	}
	return f.put(ctx, rel, strings.NewReader(text), int64(len(text)), expectSHA, expectSHA == "")
}

// Upload stores r (size bytes) as dir/name.
func (f *Files) Upload(ctx context.Context, dir, name string, r io.Reader, size int64, overwrite bool) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	if size < 0 || size > MaxUpload {
		return "", invalid("uploads are limited to 1 GB")
	}
	d, err := Clean(dir)
	if err != nil {
		return "", err
	}
	return f.put(ctx, path.Join(d, name), r, size, "", !overwrite)
}

func (f *Files) put(ctx context.Context, rel string, r io.Reader, size int64, expect string, noClobber bool) (string, error) {
	st, err := f.Stat(ctx, rel)
	if err != nil {
		return "", err
	}
	if st.Type == "dir" {
		return "", invalid("%s is a directory", st.Path)
	}
	if err := refuseSystem(st.Real, "write"); err != nil {
		return "", err
	}
	dir, name := path.Dir(st.Real), path.Base(st.Real)
	parent, err := f.Stat(ctx, f.rel(dir))
	if err != nil {
		return "", err
	}
	if parent.Type != "dir" {
		return "", invalid("folder %s doesn't exist", f.rel(dir))
	}
	mode, uid, gid := int64(0o644), parent.UID, parent.GID
	if st.Type == "file" {
		mode, uid, gid = st.Mode, st.UID, st.GID
	}
	// Free space: keep 100 MB.
	if size > 8<<20 {
		out, err := f.run(ctx, `df -P -B1 -- "$R" | awk 'NR==2{print $4}'`, dir)
		if err == nil && atoi64(out) > 0 && atoi64(out) < size+100<<20 {
			return "", invalid("not enough free disk space")
		}
	}
	tmp := tmpName(name)
	if err := f.C.Put(ctx, dir, tmp, r, size, mode, uid, gid); err != nil {
		return "", err
	}
	out, err := f.run(ctx, placeScript, st.Real, "TMP="+tmp, "EXPECT="+expect, "NOCLOBBER="+map[bool]string{true: "1", false: "0"}[noClobber], "NOFOLLOW=1")
	return strings.TrimSpace(out), err
}

// Download streams rel as a tar archive (a file or a whole directory).
func (f *Files) Download(ctx context.Context, rel string, w io.Writer) (FileStat, error) {
	st, err := f.Stat(ctx, rel)
	if err != nil {
		return st, err
	}
	if st.Type == "" {
		return st, invalid("%s doesn't exist", st.Path)
	}
	if st.Type == "other" {
		return st, invalid("only files and folders can be downloaded")
	}
	if strings.HasPrefix(st.Real+"/", "/proc/") || strings.HasPrefix(st.Real+"/", "/sys/") || strings.HasPrefix(st.Real+"/", "/dev/") {
		return st, invalid("can't download from %s", st.Real)
	}
	return st, f.C.Get(ctx, st.Real, w)
}

var badName = regexp.MustCompile(`[/\x00]`)

func validName(n string) error {
	if n == "" || n == "." || n == ".." || len(n) > 255 || badName.MatchString(n) {
		return invalid("invalid name %q", n)
	}
	return nil
}

// systemPaths can't be deleted, moved or have their mode/owner changed.
var systemPaths = map[string]bool{}

func init() {
	for _, p := range strings.Fields(`/ /bin /boot /dev /etc /home /lib /lib32 /lib64 /libx32 /media /mnt /opt /proc /root /run /sbin /srv /sys /tmp /usr /var
/usr/bin /usr/lib /usr/sbin /usr/local /usr/share /var/lib /var/log /var/lib/docker /etc/ssh /etc/systemd /etc/apt /var/lib/dpkg /var/lib/apt`) {
		systemPaths[p] = true
	}
}

func refuseSystem(real, op string) error {
	if systemPaths[real] {
		return invalid("%s is a system folder; Docklet won't %s it", real, op)
	}
	for _, p := range []string{"/proc/", "/sys/", "/dev/"} {
		if strings.HasPrefix(real+"/", p) {
			return invalid("can't %s inside %s", op, strings.TrimSuffix(p, "/"))
		}
	}
	return nil
}

// SystemWarning returns why editing a path deserves care ("" if none).
func SystemWarning(real string) string {
	for _, p := range []string{"/etc", "/boot", "/usr", "/bin", "/sbin", "/lib", "/var/lib/docker", "/var/lib/dpkg"} {
		if real == p || strings.HasPrefix(real, p+"/") {
			return "This is a system location: a wrong change here can break the server."
		}
	}
	return ""
}

// resolved returns the real path of rel without following its last element.
func (f *Files) resolved(ctx context.Context, rel string) (string, error) {
	p, err := f.abs(rel)
	if err != nil {
		return "", err
	}
	out, err := f.run(ctx, `[ -e "$R" ] || [ -L "$R" ] || { echo "$R doesn't exist" >&2; exit 3; }; echo "$R"`, p, "NOFOLLOW=1")
	if err != nil {
		return "", err
	}
	real := strings.TrimSpace(out)
	if real == f.Root {
		return "", invalid("that's the root of this volume")
	}
	return real, nil
}

// Mkdir creates a folder (and missing parents).
func (f *Files) Mkdir(ctx context.Context, dir, name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	d, err := Clean(dir)
	if err != nil {
		return "", err
	}
	p, _ := f.abs(path.Join(d, name))
	out, err := f.run(ctx, `[ -e "$R" ] && { echo "it already exists" >&2; exit 3; }
mkdir -p -- "$R"; echo "$R"`, p, "NOFOLLOW=1")
	return f.rel(strings.TrimSpace(out)), err
}

// Rename renames rel within its folder.
func (f *Files) Rename(ctx context.Context, rel, newName string) (string, error) {
	if err := validName(newName); err != nil {
		return "", err
	}
	real, err := f.resolved(ctx, rel)
	if err != nil {
		return "", err
	}
	if err := refuseSystem(real, "rename"); err != nil {
		return "", err
	}
	dst := path.Join(path.Dir(real), newName)
	_, err = f.run(ctx, `[ -e "$DST" ] || [ -L "$DST" ] && { echo "$NEWNAME already exists" >&2; exit 3; }
mv -- "$R" "$DST"`, real, "NOFOLLOW=1", "DST="+dst, "NEWNAME="+newName)
	return f.rel(dst), err
}

// Move moves rel into the folder destDir (root-relative).
func (f *Files) Move(ctx context.Context, rel, destDir string) (string, error) {
	real, err := f.resolved(ctx, rel)
	if err != nil {
		return "", err
	}
	if err := refuseSystem(real, "move"); err != nil {
		return "", err
	}
	dd, err := f.abs(destDir)
	if err != nil {
		return "", err
	}
	out, err := f.run(ctx, `[ -d "$R" ] || { echo "destination is not a folder" >&2; exit 3; }
T="$R/$(basename -- "$SRC")"
case "$T/" in "$SRC"/*) echo "can't move a folder into itself" >&2; exit 3;; esac
[ -e "$T" ] || [ -L "$T" ] && { echo "$(basename -- "$SRC") already exists there" >&2; exit 3; }
mv -- "$SRC" "$T"; echo "$T"`, dd, "SRC="+real)
	return f.rel(strings.TrimSpace(out)), err
}

// Delete removes rel (folders recursively). It never follows a symlink.
func (f *Files) Delete(ctx context.Context, rel string) (string, error) {
	real, err := f.resolved(ctx, rel)
	if err != nil {
		return "", err
	}
	if err := refuseSystem(real, "delete"); err != nil {
		return "", err
	}
	_, err = f.run(ctx, `rm -rf --one-file-system -- "$R"`, real, "NOFOLLOW=1")
	return f.rel(real), err
}

var modeRe = regexp.MustCompile(`^0?[0-7]{3,4}$`)
var ownerRe = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$|^[0-9]{1,10}$`)

// Chmod sets an octal mode (recursive for folders when asked).
func (f *Files) Chmod(ctx context.Context, rel, mode string, recursive bool) (string, error) {
	if !modeRe.MatchString(mode) {
		return "", invalid("mode must be octal, e.g. 0644 or 755")
	}
	real, err := f.resolved(ctx, rel)
	if err != nil {
		return "", err
	}
	if err := refuseSystem(real, "change the mode of"); err != nil {
		return "", err
	}
	_, err = f.run(ctx, `[ -L "$R" ] && { echo "can't chmod a symlink" >&2; exit 3; }
if [ "$REC" = 1 ]; then chmod -R -- "$MODE" "$R"; else chmod -- "$MODE" "$R"; fi`, real, "NOFOLLOW=1", "MODE="+mode, "REC="+boolEnv(recursive))
	return f.rel(real), err
}

// Chown sets owner (and optionally group), by name or number.
func (f *Files) Chown(ctx context.Context, rel, owner, group string, recursive bool) (string, error) {
	if !ownerRe.MatchString(owner) || (group != "" && !ownerRe.MatchString(group)) {
		return "", invalid("invalid user or group")
	}
	real, err := f.resolved(ctx, rel)
	if err != nil {
		return "", err
	}
	if err := refuseSystem(real, "change the owner of"); err != nil {
		return "", err
	}
	spec := owner
	if group != "" {
		spec += ":" + group
	}
	_, err = f.run(ctx, `if [ "$REC" = 1 ]; then chown -hR -- "$SPEC" "$R"; else chown -h -- "$SPEC" "$R"; fi`, real, "NOFOLLOW=1", "SPEC="+spec, "REC="+boolEnv(recursive))
	return f.rel(real), err
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
