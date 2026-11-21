// Package dockertest provides an in-memory Docker for tests. Images whose
// name contains "crash" exit right after start; "nopull" and "missing" fail
// to pull ("nopull" exists locally).
package dockertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// Container is a fake container.
type Container struct {
	files    map[string][]byte
	Spec     docker.ContainerSpec
	running  bool
	ip       string
	exit     int
	logs     []string
	restarts int
	done     chan struct{} // closed when a RunFunc container exits
}

// Fake is an in-memory Docker for tests. Images whose name contains "crash" exit
// immediately after start; "nopull" images fail to pull but exist locally.
type Fake struct {
	mu         sync.Mutex
	images     map[string]docker.Image
	containers map[string]*Container
	networks   map[string]bool
	attached   map[string]bool
	volumes    map[string]map[string][]byte
	exec       ExecFunc
	run        RunFunc
	seq        int
}

// RunResult describes how a container started under a RunFunc behaves.
type RunResult struct {
	Exit     int
	Logs     []string
	Duration time.Duration // how long it runs before exiting
	Forever  bool          // runs until stopped
}

// RunFunc decides the behaviour of a starting container; ok=false keeps the
// default (a long-running server).
type RunFunc func(spec docker.ContainerSpec) (r RunResult, ok bool)

// SetRun installs container run emulation (one-off jobs, crashing workers).
func (f *Fake) SetRun(fn RunFunc) {
	f.mu.Lock()
	f.run = fn
	f.mu.Unlock()
}

// Crash makes a running container exit with code and, like Docker's restart
// policy, restart it unless the policy is "no".
func (f *Fake) Crash(id string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.get(id)
	if err != nil {
		return
	}
	if c.Spec.RestartPolicy == "no" || c.Spec.RestartPolicy == "" {
		c.running, c.exit = false, code
		return
	}
	c.exit = code
	c.restarts++
}

func New() *Fake {
	return &Fake{images: map[string]docker.Image{}, containers: map[string]*Container{}, networks: map[string]bool{}}
}

func (f *Fake) AddImage(ref string, port int) {
	var img docker.Image
	img.ID = "sha256:" + ref
	img.Config.ExposedPorts = map[string]struct{}{fmt.Sprintf("%d/tcp", port): {}}
	f.images[ref] = img
}

// Ping implements apps.Docker.
func (f *Fake) Ping(context.Context) (string, error) { return "1.47", nil }
func (f *Fake) ImagePull(_ context.Context, ref string, p func(string)) error {
	if strings.Contains(ref, "nopull") || strings.Contains(ref, "missing") {
		return errors.New("registry unreachable")
	}
	if p != nil {
		p("Pull complete")
	}
	return nil
}
func (f *Fake) ImageInspect(_ context.Context, ref string) (docker.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img, ok := f.images[ref]; ok {
		return img, nil
	}
	for _, img := range f.images { // lookup by image id
		if img.ID == ref {
			return img, nil
		}
	}
	return docker.Image{}, docker.ErrNotFound
}

// ImageRemove deletes a tag; images used by a container are kept (conflict).
func (f *Fake) ImageRemove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	refs := []string{ref}
	img, ok := f.images[ref]
	if !ok { // by image ID: every tag of it
		refs = nil
		for r, im := range f.images {
			if im.ID == ref {
				refs, img, ok = append(refs, r), im, true
			}
		}
		if !ok {
			return nil
		}
	}
	for _, c := range f.containers {
		for _, r := range refs {
			if c.Spec.Image == r || c.Spec.Image == img.ID {
				return docker.ErrConflict
			}
		}
	}
	for _, r := range refs {
		delete(f.images, r)
	}
	return nil
}

// HasImage reports whether a tag exists.
func (f *Fake) HasImage(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.images[ref]
	return ok
}
func (f *Fake) NetworkEnsure(_ context.Context, name string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = true
	return nil
}

// NetworkAttach records attachments as "network/container".
func (f *Fake) NetworkAttach(_ context.Context, network, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.networks[network] {
		return docker.ErrNotFound
	}
	if f.attached == nil {
		f.attached = map[string]bool{}
	}
	f.attached[network+"/"+container] = true
	return nil
}

// Attached reports whether container was attached to network.
func (f *Fake) Attached(network, container string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attached[network+"/"+container]
}

func (f *Fake) NetworkRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, name)
	return nil
}
func (f *Fake) ContainerCreate(_ context.Context, s docker.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.Spec.Name == s.Name {
			return "", docker.ErrConflict
		}
	}
	f.seq++
	id := fmt.Sprintf("c%03d", f.seq)
	f.containers[id] = &Container{Spec: s}
	return id, nil
}
func (f *Fake) get(id string) (*Container, error) {
	for cid, c := range f.containers {
		if cid == id || c.Spec.Name == id {
			return c, nil
		}
	}
	return nil, docker.ErrNotFound
}
func (f *Fake) ContainerStart(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.get(id)
	if err != nil {
		return err
	}
	if strings.Contains(c.Spec.Image, "crash") {
		c.running, c.exit = false, 1
		return nil
	}
	c.running = true
	c.ip = fmt.Sprintf("10.0.0.%d", f.seq)
	if f.run != nil {
		if r, ok := f.run(c.Spec); ok {
			c.logs, c.done = r.Logs, make(chan struct{})
			if !r.Forever {
				done := c.done
				go func() {
					time.Sleep(r.Duration)
					f.mu.Lock()
					defer f.mu.Unlock()
					if c.running {
						c.running, c.exit = false, r.Exit
						close(done)
					}
				}()
			}
		}
	}
	return nil
}

// ContainerWait implements the wait endpoint (condition not-running).
func (f *Fake) ContainerWait(ctx context.Context, id string) (int, error) {
	for {
		f.mu.Lock()
		c, err := f.get(id)
		if err != nil {
			f.mu.Unlock()
			return 0, err
		}
		if !c.running {
			code := c.exit
			f.mu.Unlock()
			return code, nil
		}
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
func (f *Fake) ContainerStop(_ context.Context, id string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.get(id)
	if err != nil {
		return err
	}
	if c.running && c.done != nil {
		c.exit = 137
		close(c.done)
		c.done = nil
	}
	c.running = false
	return nil
}
func (f *Fake) ContainerRestart(ctx context.Context, id string, t int) error {
	return f.ContainerStart(ctx, id)
}
func (f *Fake) ContainerRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for cid, c := range f.containers {
		if cid == id || c.Spec.Name == id {
			delete(f.containers, cid)
		}
	}
	return nil
}
func (f *Fake) ContainerInspect(_ context.Context, id string) (docker.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var info docker.ContainerInfo
	c, err := f.get(id)
	if err != nil {
		return info, err
	}
	info.ID = id
	info.State.Running = c.running
	info.State.ExitCode = c.exit
	info.RestartCount = c.restarts
	info.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{c.Spec.Network: {IPAddress: c.ip}}
	return info, nil
}
func (f *Fake) ContainerList(_ context.Context, labels map[string]string) ([]docker.ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.ContainerSummary
outer:
	for id, c := range f.containers {
		for k, v := range labels {
			if c.Spec.Labels[k] != v {
				continue outer
			}
		}
		out = append(out, docker.ContainerSummary{ID: id, Labels: c.Spec.Labels})
	}
	return out, nil
}
func (f *Fake) ContainerLogs(_ context.Context, id string, _ docker.LogOptions, fn func(docker.LogLine) error) error {
	f.mu.Lock()
	var lines []string
	if c, err := f.get(id); err == nil {
		lines = append(lines, c.logs...)
	}
	f.mu.Unlock()
	if lines == nil {
		return fn(docker.LogLine{Stream: "stderr", Text: "boom: config missing"})
	}
	for _, l := range lines {
		if err := fn(docker.LogLine{Stream: "stdout", Text: l}); err != nil {
			return err
		}
	}
	return nil
}
func (f *Fake) Running() []*Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Container
	for _, c := range f.containers {
		if c.running {
			out = append(out, c)
		}
	}
	return out
}

// Has reports whether a container with this id or name exists.
func (f *Fake) Has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.get(id)
	return err == nil
}

// Count returns the number of containers (running or not).
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}

// ExecFunc emulates a command inside a container. It may read and write the
// container's files through f.ReadFile / f.WriteFile.
type ExecFunc func(f *Fake, containerID string, cmd, env []string, stdout io.Writer) (exitCode int, stderr string)

// SetExec installs the exec emulation.
func (f *Fake) SetExec(fn ExecFunc) {
	f.mu.Lock()
	f.exec = fn
	f.mu.Unlock()
}

// VolumeCreate implements the Docker client method.
func (f *Fake) VolumeCreate(_ context.Context, name string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumes == nil {
		f.volumes = map[string]map[string][]byte{}
	}
	if f.volumes[name] == nil {
		f.volumes[name] = map[string][]byte{}
	}
	return nil
}

// VolumeRemove implements the Docker client method.
func (f *Fake) VolumeRemove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
	return nil
}

// HasVolume reports whether a volume exists.
func (f *Fake) HasVolume(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.volumes[name]
	return ok
}

// Exec implements the Docker client method using SetExec's function.
func (f *Fake) Exec(_ context.Context, id string, cmd, env []string, stdout io.Writer) (docker.ExecResult, error) {
	f.mu.Lock()
	c, err := f.get(id)
	fn := f.exec
	f.mu.Unlock()
	if err != nil {
		return docker.ExecResult{}, err
	}
	if !c.running {
		return docker.ExecResult{}, fmt.Errorf("%w: container is not running", docker.ErrConflict)
	}
	if fn == nil {
		return docker.ExecResult{}, nil
	}
	if stdout == nil {
		stdout = io.Discard
	}
	code, stderr := fn(f, id, cmd, env, stdout)
	return docker.ExecResult{ExitCode: code, Stderr: stderr}, nil
}

// storage returns the map holding path inside a container: a volume when
// path is under a mount, otherwise the container's own filesystem.
func (f *Fake) storage(c *Container, path string) (map[string][]byte, string) {
	for vol, target := range c.Spec.Volumes {
		if path == target || strings.HasPrefix(path, target+"/") {
			if f.volumes == nil {
				f.volumes = map[string]map[string][]byte{}
			}
			if f.volumes[vol] == nil {
				f.volumes[vol] = map[string][]byte{}
			}
			return f.volumes[vol], path
		}
	}
	if c.files == nil {
		c.files = map[string][]byte{}
	}
	return c.files, path
}

// WriteFile stores a file inside a container (for ExecFunc emulations).
func (f *Fake) WriteFile(id, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.get(id)
	if err != nil {
		return err
	}
	m, p := f.storage(c, path)
	m[p] = append([]byte(nil), data...)
	return nil
}

// ReadFile reads a file inside a container.
func (f *Fake) ReadFile(id, path string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.get(id)
	if err != nil {
		return nil, false
	}
	m, p := f.storage(c, path)
	b, ok := m[p]
	return b, ok
}

// CopyTo implements the Docker client method.
func (f *Fake) CopyTo(_ context.Context, id, dir, name string, content io.Reader, _ int64, _ int64) error {
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	return f.WriteFile(id, strings.TrimSuffix(dir, "/")+"/"+name, b)
}

// CopyFrom implements the Docker client method.
func (f *Fake) CopyFrom(_ context.Context, id, path string, w io.Writer) (int64, error) {
	b, ok := f.ReadFile(id, path)
	if !ok {
		return 0, docker.ErrNotFound
	}
	n, err := w.Write(b)
	return int64(n), err
}

// DiskUsage reports the fake's images (with container counts), containers
// and volumes.
func (f *Fake) DiskUsage(context.Context) (docker.DiskUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var du docker.DiskUsage
	uses := map[string]int64{}
	for _, c := range f.containers {
		if img, ok := f.images[c.Spec.Image]; ok {
			uses[img.ID]++
		}
	}
	for ref, img := range f.images {
		du.Images = append(du.Images, struct {
			ID         string   `json:"Id"`
			RepoTags   []string `json:"RepoTags"`
			Size       int64    `json:"Size"`
			SharedSize int64    `json:"SharedSize"`
			Containers int64    `json:"Containers"`
			Created    int64    `json:"Created"`
		}{ID: img.ID, RepoTags: []string{ref}, Size: 10 << 20, Containers: uses[img.ID]})
	}
	for name := range f.volumes {
		du.Volumes = append(du.Volumes, struct {
			Name      string            `json:"Name"`
			Labels    map[string]string `json:"Labels"`
			UsageData struct {
				Size     int64 `json:"Size"`
				RefCount int64 `json:"RefCount"`
			} `json:"UsageData"`
		}{Name: name})
	}
	return du, nil
}

// BuildPrune pretends to remove the build cache.
func (f *Fake) BuildPrune(context.Context) (int64, error) { return 0, nil }
