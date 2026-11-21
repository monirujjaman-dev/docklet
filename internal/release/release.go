// Package release looks up Docklet releases on GitHub and installs release
// binaries with checksum verification.
package release

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is where releases are published.
const DefaultRepo = "monirujjaman-dev/docklet"

// ChecksumsAsset is the sha256sum-format file attached to every release.
const ChecksumsAsset = "checksums.txt"

// Release is a published GitHub release.
type Release struct {
	Tag         string            `json:"tag"`
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	PublishedAt time.Time         `json:"publishedAt"`
	Assets      map[string]string `json:"-"` // name -> download URL
}

// Client talks to the GitHub API.
type Client struct {
	HTTP    *http.Client
	APIBase string // https://api.github.com
	Repo    string
}

// New returns a client with sane timeouts.
func New(repo string) *Client {
	if repo == "" {
		repo = DefaultRepo
	}
	return &Client{HTTP: &http.Client{Timeout: 5 * time.Minute}, APIBase: "https://api.github.com", Repo: repo}
}

// Latest returns the newest non-prerelease release, or a specific tag.
func (c *Client) Latest(ctx context.Context, tag string) (Release, error) {
	path := "/releases/latest"
	if tag != "" {
		path = "/releases/tags/" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+"/repos/"+c.Repo+path, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("contact GitHub: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return Release{}, errors.New("no published release found")
	}
	if res.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub API: %s", res.Status)
	}
	var body struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("GitHub API: %w", err)
	}
	r := Release{Tag: body.TagName, Name: body.Name, URL: body.HTMLURL, PublishedAt: body.PublishedAt, Assets: map[string]string{}}
	for _, a := range body.Assets {
		r.Assets[a.Name] = a.URL
	}
	return r, nil
}

// AssetName is the release binary for this platform.
func AssetName() string { return "docklet-" + runtime.GOOS + "-" + runtime.GOARCH }

// Compare compares two "vX.Y.Z" versions. ok is false when either isn't a
// plain release version (e.g. "main" or "v1.0.0-3-gabc123").
func Compare(a, b string) (cmp int, ok bool) {
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func (c *Client) get(ctx context.Context, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, res.Status)
	}
	n, err := io.Copy(w, io.LimitReader(res.Body, limit+1))
	if err == nil && n > limit {
		err = fmt.Errorf("download %s: larger than %d bytes", url, limit)
	}
	return err
}

// Checksum returns the expected sha256 of asset from the release's checksums file.
func (c *Client) Checksum(ctx context.Context, r Release, asset string) (string, error) {
	url, ok := r.Assets[ChecksumsAsset]
	if !ok {
		return "", fmt.Errorf("release %s has no %s", r.Tag, ChecksumsAsset)
	}
	var sb strings.Builder
	if err := c.get(ctx, url, &sb, 1<<20); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(sb.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s lists no checksum for %s", ChecksumsAsset, asset)
}

// Install downloads the platform binary of r, verifies its checksum and that
// it runs, and atomically replaces exe. The old binary is kept as
// <exe>.previous. It returns the new binary's `version` output.
func (c *Client) Install(ctx context.Context, r Release, exe string) (string, error) {
	asset := AssetName()
	url, ok := r.Assets[asset]
	if !ok {
		return "", fmt.Errorf("release %s has no binary for %s/%s", r.Tag, runtime.GOOS, runtime.GOARCH)
	}
	want, err := c.Checksum(ctx, r, asset)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".docklet-upgrade-")
	if err != nil {
		return "", fmt.Errorf("write next to %s: %w", exe, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	err = c.get(ctx, url, io.MultiWriter(tmp, h), 200<<20)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // an executable
		return "", err
	}
	out, err := exec.CommandContext(ctx, tmp.Name(), "version").CombinedOutput() //nolint:gosec // verified binary
	if err != nil {
		return "", fmt.Errorf("the new binary does not run: %w %s", err, out)
	}
	prev := exe + ".previous"
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil {
		if b, rerr := os.ReadFile(exe); rerr == nil { //nolint:gosec // our executable
			_ = os.WriteFile(prev, b, 0o755) //nolint:gosec // an executable
		}
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return "", fmt.Errorf("replace %s: %w", exe, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// InContainer reports whether this process runs in a container, where the
// image, not the binary, must be upgraded.
func InContainer() bool {
	if os.Getenv("DOCKLET_CONTAINER") == "1" {
		return true
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}
