package host

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/monirujjaman-dev/docklet/internal/docker"
)

// CleanupDocker is what disk cleanup needs from Docker.
type CleanupDocker interface {
	DiskUsage(ctx context.Context) (docker.DiskUsage, error)
	ImageRemove(ctx context.Context, ref string) error
	BuildPrune(ctx context.Context) (int64, error)
	ContainerRemove(ctx context.Context, id string) error
	VolumeRemove(ctx context.Context, name string) error
}

// CleanupItem is one thing that can be removed.
type CleanupItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Detail string `json:"detail,omitempty"`
}

// CleanupCategory groups items; Size is the estimated space freed.
type CleanupCategory struct {
	Key   string        `json:"key"`
	Label string        `json:"label"`
	Size  int64         `json:"size"`
	Items []CleanupItem `json:"items,omitempty"`
	OptIn bool          `json:"optIn,omitempty"` // only listed items the admin ticks
	Note  string        `json:"note,omitempty"`
}

// Never removed: Docklet's own images and Caddy's.
func protectedImage(tags []string) bool {
	for _, t := range tags {
		if strings.Contains(t, "/docklet:") || strings.HasPrefix(t, "caddy:") || strings.Contains(t, "/caddy:") {
			return true
		}
	}
	return false
}

func dockletOwned(labels map[string]string, name string) bool {
	for k := range labels {
		if strings.HasPrefix(k, "docklet.") {
			return true
		}
	}
	return labels["com.docker.compose.project"] == "docklet" || strings.HasPrefix(name, "docklet")
}

// journalctl --disk-usage: "Archived and active journals take up 24.0M in the file system."
var journalRe = regexp.MustCompile(`take up ([0-9.]+)([KMGT]?)`)

const journalKeep = 100 << 20 // vacuum the journal down to 100 MB

// CleanupPreview lists what can be freed. keepImages are image IDs that must
// stay (images of current and previous deployments, for rollback).
func (s *Service) CleanupPreview(ctx context.Context, dk CleanupDocker, keepImages map[string]bool) ([]CleanupCategory, error) {
	du, err := dk.DiskUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker disk usage: %w", err)
	}
	images := CleanupCategory{Key: "images", Label: "Unused images", Note: "Not used by any container. Images of current and previous deployments (for rollback), Docklet and Caddy are kept."}
	for _, im := range du.Images {
		if im.Containers != 0 || keepImages[im.ID] || protectedImage(im.RepoTags) {
			continue
		}
		size := im.Size - im.SharedSize
		if im.SharedSize < 0 || size < 0 {
			size = im.Size
		}
		name := strings.Join(im.RepoTags, ", ")
		if name == "" || name == "<none>:<none>" {
			name = "<untagged> " + shortID(im.ID)
		}
		images.Items = append(images.Items, CleanupItem{ID: im.ID, Name: name, Size: size})
		images.Size += size
	}
	cache := CleanupCategory{Key: "build-cache", Label: "Build cache", Note: "Layers cached by Git/Dockerfile builds; the next build may take longer."}
	for _, b := range du.BuildCache {
		if !b.InUse {
			cache.Size += b.Size
		}
	}
	containers := CleanupCategory{Key: "containers", Label: "Stopped containers not managed by Docklet"}
	for _, c := range du.Containers {
		name := strings.TrimPrefix(strings.Join(c.Names, ","), "/")
		if c.State == "running" || c.State == "restarting" || c.State == "paused" || dockletOwned(c.Labels, name) {
			continue
		}
		containers.Items = append(containers.Items, CleanupItem{ID: c.ID, Name: name, Size: c.SizeRw, Detail: c.Image + " · " + c.State})
		containers.Size += c.SizeRw
	}
	volumes := CleanupCategory{Key: "volumes", Label: "Unused volumes", OptIn: true,
		Note: "Not attached to any container. They may still hold data: tick only the ones you are sure about. Docklet's volumes are never listed."}
	for _, v := range du.Volumes {
		if v.UsageData.RefCount != 0 || dockletOwned(v.Labels, v.Name) {
			continue
		}
		size := v.UsageData.Size
		if size < 0 {
			size = 0
		}
		volumes.Items = append(volumes.Items, CleanupItem{ID: v.Name, Name: v.Name, Size: size})
	}
	for _, c := range []*CleanupCategory{&images, &containers, &volumes} {
		sort.Slice(c.Items, func(i, j int) bool { return c.Items[i].Size > c.Items[j].Size })
	}
	cats := []CleanupCategory{images, cache, containers, volumes}

	if err := s.requireSupported(ctx); err == nil {
		out, err := Script(ctx, s.R, `journalctl --disk-usage 2>/dev/null; echo; du -sb /var/cache/apt/archives 2>/dev/null | cut -f1; true`)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(out), "\n")
			j := CleanupCategory{Key: "journal", Label: "System journal", Note: "Old system logs; the newest 100 MB are kept."}
			if m := journalRe.FindStringSubmatch(out); m != nil {
				if size := parseSize(m[1], m[2]); size > journalKeep {
					j.Size = size - journalKeep
				}
			}
			a := CleanupCategory{Key: "apt", Label: "Package cache", Note: "Downloaded .deb files (apt-get clean)."}
			if n := atoi64(lines[len(lines)-1]); n > 4096 {
				a.Size = n - 4096
			}
			cats = append(cats, j, a)
		}
	}
	return cats, nil
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func parseSize(num, unit string) int64 {
	f, _ := strconv.ParseFloat(num, 64)
	mult := map[string]float64{"": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}[unit]
	return int64(f * mult)
}

// CleanupResult reports what one category freed.
type CleanupResult struct {
	Key     string   `json:"key"`
	Removed int      `json:"removed"`
	Freed   int64    `json:"freed"` // estimate
	Errors  []string `json:"errors,omitempty"`
}

// Cleanup removes the selected categories. Volumes are removed only if named
// in volumes and still unused. The preview is recomputed first so nothing
// that became used in the meantime is removed.
func (s *Service) Cleanup(ctx context.Context, dk CleanupDocker, keepImages map[string]bool, keys []string, volumes []string) ([]CleanupResult, error) {
	cats, err := s.CleanupPreview(ctx, dk, keepImages)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	pickVol := map[string]bool{}
	for _, v := range volumes {
		pickVol[v] = true
	}
	byKey := map[string]CleanupCategory{}
	for _, c := range cats {
		byKey[c.Key] = c
	}
	var results []CleanupResult
	run := func(key string, fn func(r *CleanupResult)) {
		if !want[key] {
			return
		}
		r := CleanupResult{Key: key}
		fn(&r)
		results = append(results, r)
	}
	// Containers first: they may hold references to images.
	run("containers", func(r *CleanupResult) {
		for _, it := range byKey["containers"].Items {
			if err := dk.ContainerRemove(ctx, it.ID); err != nil {
				r.Errors = append(r.Errors, it.Name+": "+err.Error())
				continue
			}
			r.Removed++
			r.Freed += it.Size
		}
	})
	run("images", func(r *CleanupResult) {
		for _, it := range byKey["images"].Items {
			if err := dk.ImageRemove(ctx, it.ID); err != nil {
				r.Errors = append(r.Errors, it.Name+": "+err.Error())
				continue
			}
			r.Removed++
			r.Freed += it.Size
		}
	})
	run("build-cache", func(r *CleanupResult) {
		n, err := dk.BuildPrune(ctx)
		if err != nil {
			r.Errors = append(r.Errors, err.Error())
			return
		}
		r.Freed = n
	})
	run("volumes", func(r *CleanupResult) {
		for _, it := range byKey["volumes"].Items {
			if !pickVol[it.ID] {
				continue
			}
			if err := dk.VolumeRemove(ctx, it.ID); err != nil {
				r.Errors = append(r.Errors, it.Name+": "+err.Error())
				continue
			}
			r.Removed++
			r.Freed += it.Size
		}
	})
	run("journal", func(r *CleanupResult) {
		if _, ok := byKey["journal"]; !ok {
			r.Errors = append(r.Errors, "not available on this host")
			return
		}
		if _, err := Check(ctx, s.R, []string{"journalctl", "--vacuum-size=100M"}); err != nil {
			r.Errors = append(r.Errors, err.Error())
			return
		}
		r.Freed = byKey["journal"].Size
	})
	run("apt", func(r *CleanupResult) {
		if _, ok := byKey["apt"]; !ok {
			r.Errors = append(r.Errors, "not available on this host")
			return
		}
		if _, err := Check(ctx, s.R, []string{"apt-get", "clean"}); err != nil {
			r.Errors = append(r.Errors, err.Error())
			return
		}
		r.Freed = byKey["apt"].Size
	})
	return results, nil
}
