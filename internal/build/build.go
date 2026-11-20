// Package build clones a Git repository and builds its Dockerfile with the
// docker CLI (BuildKit), streaming output line by line.
//
// The CLI is used instead of the Engine API because BuildKit over the API
// needs a gRPC session; the CLI handles that, and supports every Dockerfile
// feature (cache mounts, heredocs, # syntax=).
package build

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Spec describes one build.
type Spec struct {
	RepoURL    string
	Branch     string
	Context    string // directory inside the repo, e.g. "." or "services/api"
	Dockerfile string // path inside the repo, e.g. "Dockerfile"
	PrivateKey []byte // OpenSSH private key for SSH URLs (optional)
	Tag        string // image tag to produce
	Labels     map[string]string
}

// Result describes the built commit.
type Result struct {
	CommitSHA     string
	CommitMessage string
}

// Builder runs builds one at a time (by default) under a work directory.
type Builder struct {
	DockerHost string // e.g. unix:///var/run/docker.sock
	WorkDir    string // scratch space for checkouts
	KnownHosts string // SSH known_hosts file (trust on first use)
	Timeout    time.Duration

	sem chan struct{}
}

// New returns a builder allowing `concurrency` simultaneous builds.
func New(dockerHost, workDir string, concurrency int, timeout time.Duration) *Builder {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Builder{
		DockerHost: dockerHost, WorkDir: workDir, KnownHosts: filepath.Join(workDir, "known_hosts"),
		Timeout: timeout, sem: make(chan struct{}, concurrency),
	}
}

// Available reports a helpful error if git or docker are missing.
func (b *Builder) Available() error {
	var missing []string
	for _, bin := range []string{"git", "docker"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("builds need %s on this host (install git, openssh-client and the docker CLI with buildx)", strings.Join(missing, " and "))
	}
	return nil
}

var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._~/-]+$`)

// ValidateRepoURL accepts https://, http://, ssh:// and git@host:path URLs.
// Credentials in URLs are rejected: use a deploy key instead, so secrets are
// never stored in plain text or printed in build logs.
func ValidateRepoURL(raw string) error {
	if raw == "" || len(raw) > 512 || strings.HasPrefix(raw, "-") || strings.ContainsAny(raw, " \t\n\r") {
		return errors.New("invalid repository URL")
	}
	if scpLike.MatchString(raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("repository URL must look like https://github.com/org/repo.git or git@github.com:org/repo.git")
	}
	switch u.Scheme {
	case "https", "http", "ssh":
	default:
		return fmt.Errorf("unsupported repository URL scheme %q", u.Scheme)
	}
	if _, hasPw := u.User.Password(); hasPw || (u.Scheme != "ssh" && u.User != nil) {
		return errors.New("don't put credentials in the URL; use the app's SSH deploy key for private repositories")
	}
	return nil
}

// IsSSH reports whether the URL is cloned over SSH.
func IsSSH(raw string) bool {
	return strings.HasPrefix(raw, "ssh://") || scpLike.MatchString(raw)
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// ValidateBranch rejects names git would treat as options or that are unsafe.
func ValidateBranch(b string) error {
	if !branchRe.MatchString(b) || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return errors.New("invalid branch name")
	}
	return nil
}

// ValidatePath checks a repository-relative path (context or Dockerfile).
func ValidatePath(p string) error {
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "-") {
		return errors.New("must be a path inside the repository")
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return errors.New("must not leave the repository")
	}
	return nil
}

// Build clones spec.RepoURL at spec.Branch and builds spec.Tag. Every output
// line goes to logf. Builds beyond the concurrency limit wait their turn.
func (b *Builder) Build(ctx context.Context, spec Spec, logf func(string)) (Result, error) {
	var res Result
	if err := b.Available(); err != nil {
		return res, err
	}
	for _, check := range []error{ValidateRepoURL(spec.RepoURL), ValidateBranch(spec.Branch), ValidatePath(spec.Context), ValidatePath(spec.Dockerfile)} {
		if check != nil {
			return res, check
		}
	}

	select {
	case b.sem <- struct{}{}:
	default:
		logf("Waiting for another build to finish…")
		select {
		case b.sem <- struct{}{}:
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
	defer func() { <-b.sem }()

	if b.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.Timeout)
		defer cancel()
	}
	if err := os.MkdirAll(b.WorkDir, 0o700); err != nil {
		return res, err
	}
	dir, err := os.MkdirTemp(b.WorkDir, "build-")
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	src := filepath.Join(dir, "src")

	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/true",
		"DOCKER_HOST=" + b.DockerHost, "DOCKER_BUILDKIT=1",
	}
	if IsSSH(spec.RepoURL) {
		if len(spec.PrivateKey) == 0 {
			return res, errors.New("this SSH URL needs the app's deploy key")
		}
		keyFile := filepath.Join(dir, "deploy_key")
		if err := os.WriteFile(keyFile, spec.PrivateKey, 0o600); err != nil {
			return res, err
		}
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+keyFile+
			" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new"+
			" -o UserKnownHostsFile="+b.KnownHosts)
	}

	logf(fmt.Sprintf("Cloning %s (branch %s)", spec.RepoURL, spec.Branch))
	if err := run(ctx, env, "", logf, "git", "clone", "--depth", "1", "--single-branch",
		"--branch", spec.Branch, "--", spec.RepoURL, src); err != nil {
		return res, fmt.Errorf("git clone: %w", err)
	}
	out, err := output(ctx, env, src, "git", "log", "-1", "--format=%H%n%s")
	if err != nil {
		return res, fmt.Errorf("git log: %w", err)
	}
	sha, msg, _ := strings.Cut(strings.TrimSpace(out), "\n")
	res.CommitSHA, res.CommitMessage = sha, truncate(msg, 200)
	logf(fmt.Sprintf("Commit %s: %s", shortSHA(sha), res.CommitMessage))

	contextDir := filepath.Join(src, filepath.Clean(spec.Context))
	dockerfile := filepath.Join(src, filepath.Clean(spec.Dockerfile))
	if st, err := os.Stat(contextDir); err != nil || !st.IsDir() {
		return res, fmt.Errorf("build context %q not found in the repository", spec.Context)
	}
	if _, err := os.Stat(dockerfile); err != nil {
		return res, fmt.Errorf("%s not found in the repository (builds without a Dockerfile are not supported yet)", spec.Dockerfile)
	}

	args := []string{"build", "--progress=plain", "-t", spec.Tag, "-f", dockerfile}
	keys := make([]string, 0, len(spec.Labels))
	for k := range spec.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
	args = append(args, "--label", "org.opencontainers.image.revision="+sha, contextDir)

	logf("Building image " + spec.Tag)
	if err := run(ctx, env, src, logf, "docker", args...); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("build timed out after %s", b.Timeout)
		}
		return res, fmt.Errorf("docker build: %w", err)
	}
	return res, nil
}

// run executes a command, streaming stdout+stderr lines to logf.
func run(ctx context.Context, env []string, dir string, logf func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed binaries; inputs validated
	cmd.Env, cmd.Dir = env, dir
	cmd.WaitDelay = 5 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			logf("  " + sc.Text())
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	err := cmd.Wait()
	_ = pw.Close()
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func output(ctx context.Context, env []string, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed binaries
	cmd.Env, cmd.Dir = env, dir
	b, err := cmd.Output()
	return string(b), err
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
