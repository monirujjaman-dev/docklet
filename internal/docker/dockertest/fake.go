// Package dockertest provides an in-memory Docker for tests. Images whose
// name contains "crash" exit right after start; "nopull" and "missing" fail
// to pull ("nopull" exists locally).
package dockertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// Container is a fake container.
type Container struct {
	Spec    docker.ContainerSpec
	running bool
	ip      string
	exit    int
}

// Fake is an in-memory Docker for tests. Images whose name contains "crash" exit
// immediately after start; "nopull" images fail to pull but exist locally.
type Fake struct {
	mu         sync.Mutex
	images     map[string]docker.Image
	containers map[string]*Container
	networks   map[string]bool
	attached   map[string]bool
	seq        int
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
	p("Pull complete")
	return nil
}
func (f *Fake) ImageInspect(_ context.Context, ref string) (docker.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	img, ok := f.images[ref]
	if !ok {
		return img, docker.ErrNotFound
	}
	return img, nil
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
	return nil
}
func (f *Fake) ContainerStop(_ context.Context, id string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.get(id)
	if err != nil {
		return err
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
func (f *Fake) ContainerLogs(_ context.Context, _ string, _ docker.LogOptions, fn func(docker.LogLine) error) error {
	return fn(docker.LogLine{Stream: "stderr", Text: "boom: config missing"})
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
