package docker

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SplitImageRef splits "repo[:tag]" or "repo@digest" for the pull API.
// The tag defaults to "latest".
func SplitImageRef(ref string) (image, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref, ""
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

// ImagePull pulls ref, reporting progress lines to progress (may be nil).
func (c *Client) ImagePull(ctx context.Context, ref string, progress func(string)) error {
	image, tag := SplitImageRef(ref)
	q := url.Values{"fromImage": {image}}
	if tag != "" {
		q.Set("tag", tag)
	}
	res, err := c.request(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	dec := json.NewDecoder(res.Body)
	last := ""
	for {
		var m struct {
			Status      string `json:"status"`
			ID          string `json:"id"`
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker: pull %s: %w", ref, err)
		}
		if m.Error != "" || m.ErrorDetail.Message != "" {
			msg := m.ErrorDetail.Message
			if msg == "" {
				msg = m.Error
			}
			return fmt.Errorf("docker: pull %s: %s", ref, msg)
		}
		// Per-layer "Downloading" updates are noisy; report status changes only.
		line := m.Status
		if m.ID != "" {
			line = m.ID + ": " + m.Status
		}
		if progress != nil && line != last && !strings.HasPrefix(m.Status, "Downloading") && !strings.HasPrefix(m.Status, "Extracting") {
			progress(line)
		}
		last = line
	}
}

// Image is the subset of image inspect Docklet uses.
type Image struct {
	ID     string `json:"Id"`
	Config struct {
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
}

// FirstTCPPort returns the lowest exposed TCP port, or 0.
func (i Image) FirstTCPPort() int {
	best := 0
	for p := range i.Config.ExposedPorts {
		num, proto, _ := strings.Cut(p, "/")
		if proto != "" && proto != "tcp" {
			continue
		}
		if n, err := strconv.Atoi(num); err == nil && (best == 0 || n < best) {
			best = n
		}
	}
	return best
}

// ImageInspect returns image metadata.
func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	var img Image
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &img)
	return img, err
}

// ImageRemove deletes an image by tag or id (not found is not an error).
// An image still used by a container yields ErrConflict.
func (c *Client) ImageRemove(ctx context.Context, ref string) error {
	err := c.do(ctx, http.MethodDelete, "/images/"+url.PathEscape(ref), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// NetworkEnsure creates a bridge network if it does not exist.
func (c *Client) NetworkEnsure(ctx context.Context, name string, labels map[string]string) error {
	err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, nil)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	err = c.do(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "Labels": labels, "CheckDuplicate": true,
	}, nil)
	if errors.Is(err, ErrConflict) {
		return nil // created concurrently
	}
	return err
}

type networkInfo struct {
	ID         string `json:"Id"`
	Containers map[string]struct {
		Name string `json:"Name"`
	} `json:"Containers"`
}

func (c *Client) networkInspect(ctx context.Context, name string) (networkInfo, error) {
	var n networkInfo
	err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, &n)
	return n, err
}

// NetworkAttach connects a container (name or id) to a network unless it is
// already connected. Used when Docklet and Caddy themselves run in containers
// and must reach app containers on project networks.
func (c *Client) NetworkAttach(ctx context.Context, network, container string) error {
	n, err := c.networkInspect(ctx, network)
	if err != nil {
		return err
	}
	for id, ep := range n.Containers {
		if ep.Name == container || strings.HasPrefix(id, container) {
			return nil
		}
	}
	err = c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil,
		map[string]any{"Container": container}, nil)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// NetworkRemove disconnects any remaining containers and deletes the network
// (not found is not an error).
func (c *Client) NetworkRemove(ctx context.Context, name string) error {
	n, err := c.networkInspect(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for id := range n.Containers {
		if err := c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(name)+"/disconnect", nil,
			map[string]any{"Container": id, "Force": true}, nil); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	err = c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ContainerSpec describes a container to create.
type ContainerSpec struct {
	Name          string
	Image         string
	Env           []string
	Labels        map[string]string
	Hostname      string
	Port          int // container port to expose on the network
	Network       string
	Aliases       []string
	RestartPolicy string // "no", "always", "unless-stopped", "on-failure"
	NanoCPUs      int64
	MemoryBytes   int64

	Cmd     []string          // overrides the image CMD when set
	Volumes map[string]string // named volume -> mount path
	// Publish maps container TCP ports to host ports (0.0.0.0). Empty keeps
	// the container reachable only on its Docker network.
	Publish map[int]int
}

// ContainerCreate creates (but does not start) a container.
func (c *Client) ContainerCreate(ctx context.Context, s ContainerSpec) (string, error) {
	exposed := map[string]any{}
	if s.Port > 0 {
		exposed[fmt.Sprintf("%d/tcp", s.Port)] = struct{}{}
	}
	host := map[string]any{
		"RestartPolicy": map[string]any{"Name": s.RestartPolicy},
		"NetworkMode":   s.Network,
		"LogConfig": map[string]any{
			// Bounded json-file logs so containers cannot fill the disk.
			"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "3"},
		},
	}
	if s.NanoCPUs > 0 {
		host["NanoCpus"] = s.NanoCPUs
	}
	if s.MemoryBytes > 0 {
		host["Memory"] = s.MemoryBytes
	}
	if len(s.Volumes) > 0 {
		var mounts []map[string]any
		for vol, target := range s.Volumes {
			mounts = append(mounts, map[string]any{"Type": "volume", "Source": vol, "Target": target})
		}
		host["Mounts"] = mounts
	}
	if len(s.Publish) > 0 {
		bindings := map[string]any{}
		for cport, hport := range s.Publish {
			key := fmt.Sprintf("%d/tcp", cport)
			exposed[key] = struct{}{}
			bindings[key] = []map[string]string{{"HostIp": "0.0.0.0", "HostPort": strconv.Itoa(hport)}}
		}
		host["PortBindings"] = bindings
	}
	body := map[string]any{
		"Image":        s.Image,
		"Env":          s.Env,
		"Labels":       s.Labels,
		"Hostname":     s.Hostname,
		"ExposedPorts": exposed,
		"HostConfig":   host,
		"Cmd":          s.Cmd,
		"NetworkingConfig": map[string]any{
			"EndpointsConfig": map[string]any{
				s.Network: map[string]any{"Aliases": s.Aliases},
			},
		},
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {s.Name}}, body, &out)
	return out.ID, err
}

// ContainerStart starts a container (already running is not an error).
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// ContainerStop stops a container, waiting up to timeoutSec before SIGKILL.
func (c *Client) ContainerStop(ctx context.Context, id string, timeoutSec int) error {
	q := url.Values{"t": {strconv.Itoa(timeoutSec)}}
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil)
}

// ContainerRestart restarts a container.
func (c *Client) ContainerRestart(ctx context.Context, id string, timeoutSec int) error {
	q := url.Values{"t": {strconv.Itoa(timeoutSec)}}
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/restart", q, nil, nil)
}

// ContainerRemove force-removes a container (not found is not an error).
func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ContainerInfo is the subset of container inspect Docklet uses.
type ContainerInfo struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"` // created, running, paused, restarting, removing, exited, dead
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		Pid        int    `json:"Pid"`
	} `json:"State"`
	RestartCount    int `json:"RestartCount"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// IP returns the container's address on network.
func (i ContainerInfo) IP(network string) string {
	if n, ok := i.NetworkSettings.Networks[network]; ok {
		return n.IPAddress
	}
	return ""
}

// ContainerInspect returns container details.
func (c *Client) ContainerInspect(ctx context.Context, id string) (ContainerInfo, error) {
	var info ContainerInfo
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &info)
	return info, err
}

// ContainerSummary is an entry from container list.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"`
	State  string            `json:"State"`
}

// ContainerList lists all containers (running or not) having every label.
func (c *Client) ContainerList(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	var f []string
	for k, v := range labels {
		f = append(f, k+"="+v)
	}
	filters, _ := json.Marshal(map[string][]string{"label": f})
	var out []ContainerSummary
	err := c.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, nil, &out)
	return out, err
}

// LogOptions controls ContainerLogs.
type LogOptions struct {
	Follow     bool
	Tail       int // 0 = all
	Timestamps bool
}

// LogLine is one demultiplexed log line.
type LogLine struct {
	Stream string `json:"stream"` // "stdout" or "stderr"
	Text   string `json:"text"`
}

// ContainerLogs streams logs, calling fn per line until the stream ends or
// ctx is cancelled. Containers are created without a TTY, so the stream is
// multiplexed with 8-byte frame headers.
func (c *Client) ContainerLogs(ctx context.Context, id string, o LogOptions, fn func(LogLine) error) error {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if o.Follow {
		q.Set("follow", "1")
	}
	if o.Tail > 0 {
		q.Set("tail", strconv.Itoa(o.Tail))
	}
	if o.Timestamps {
		q.Set("timestamps", "1")
	}
	res, err := c.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	return Demux(res.Body, fn)
}

// Demux splits a multiplexed Docker log stream into lines.
func Demux(r io.Reader, fn func(LogLine) error) error {
	br := bufio.NewReaderSize(r, 32<<10)
	var hdr [8]byte
	partial := map[byte]string{}
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return flushPartial(partial, fn)
			}
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		if size > 1<<20 {
			return fmt.Errorf("docker: log frame too large (%d bytes)", size)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		stream := hdr[0]
		text := partial[stream] + string(buf)
		lines := strings.Split(text, "\n")
		partial[stream] = lines[len(lines)-1]
		for _, l := range lines[:len(lines)-1] {
			if err := fn(LogLine{Stream: streamName(stream), Text: strings.TrimSuffix(l, "\r")}); err != nil {
				return err
			}
		}
	}
}

func flushPartial(p map[byte]string, fn func(LogLine) error) error {
	for s, t := range p {
		if t != "" {
			if err := fn(LogLine{Stream: streamName(s), Text: t}); err != nil {
				return err
			}
		}
	}
	return nil
}

func streamName(b byte) string {
	if b == 2 {
		return "stderr"
	}
	return "stdout"
}

// Stats is the subset of the container stats response Docklet uses.
type Stats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"` // nanoseconds
		} `json:"cpu_usage"`
		OnlineCPUs int `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IOServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

// ContainerStats returns a single stats snapshot without waiting for a
// second sample (one-shot); rates are computed by the caller.
func (c *Client) ContainerStats(ctx context.Context, id string) (Stats, error) {
	var st Stats
	q := url.Values{"stream": {"false"}, "one-shot": {"true"}}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats", q, nil, &st)
	return st, err
}

// VolumeCreate creates a named volume (existing is not an error).
func (c *Client) VolumeCreate(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// VolumeRemove deletes a named volume (not found is not an error).
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ExecResult is the outcome of Exec.
type ExecResult struct {
	ExitCode int
	Stderr   string // last 64 KiB
}

// Exec runs cmd in a running container. Stdout is streamed to stdout (may be
// nil); stderr is captured. env adds variables, e.g. passwords that must not
// appear in the process list.
func (c *Client) Exec(ctx context.Context, id string, cmd, env []string, stdout io.Writer) (ExecResult, error) {
	var res ExecResult
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, map[string]any{
		"Cmd": cmd, "Env": env, "AttachStdout": true, "AttachStderr": true,
	}, &created); err != nil {
		return res, err
	}
	resp, err := c.request(ctx, http.MethodPost, "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return res, err
	}
	stderr := &tailBuffer{max: 64 << 10}
	derr := demuxTo(resp.Body, stdout, stderr)
	_ = resp.Body.Close()
	if derr != nil {
		return res, derr
	}
	var info struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := c.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &info); err != nil {
		return res, err
	}
	res.ExitCode, res.Stderr = info.ExitCode, stderr.String()
	return res, nil
}

// demuxTo copies a multiplexed stream to stdout/stderr writers.
func demuxTo(r io.Reader, stdout, stderr io.Writer) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		dst := io.Discard
		switch hdr[0] {
		case 1:
			if stdout != nil {
				dst = stdout
			}
		case 2:
			dst = stderr
		}
		if _, err := io.CopyN(dst, br, n); err != nil {
			return err
		}
	}
}

type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// CopyTo writes one file into a container (running or stopped) at dir/name.
func (c *Client) CopyTo(ctx context.Context, id, dir, name string, content io.Reader, size int64, mode int64) error {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: size, Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(tw, content)
		}
		if err == nil {
			err = tw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	p, err := c.prefix(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		p+"/containers/"+url.PathEscape(id)+"/archive?"+url.Values{"path": {dir}}.Encode(), pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("docker: copy to container: %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// CopyFrom streams one file out of a container into w.
func (c *Client) CopyFrom(ctx context.Context, id, path string, w io.Writer) (int64, error) {
	res, err := c.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	tr := tar.NewReader(res.Body)
	for {
		h, err := tr.Next()
		if err != nil {
			return 0, fmt.Errorf("docker: copy from container: %w", err)
		}
		if h.Typeflag == tar.TypeReg {
			return io.Copy(w, tr) //nolint:gosec // archive from our own container; caller bounds the destination
		}
	}
}
