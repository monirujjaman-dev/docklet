package docker

import (
	"context"
	"net/http"
	"net/url"
)

// DiskUsage is the response of GET /system/df.
type DiskUsage struct {
	Images []struct {
		ID         string   `json:"Id"`
		RepoTags   []string `json:"RepoTags"`
		Size       int64    `json:"Size"`
		SharedSize int64    `json:"SharedSize"`
		Containers int64    `json:"Containers"`
		Created    int64    `json:"Created"`
	} `json:"Images"`
	Containers []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		ImageID string            `json:"ImageID"`
		State   string            `json:"State"`
		SizeRw  int64             `json:"SizeRw"`
		Labels  map[string]string `json:"Labels"`
	} `json:"Containers"`
	Volumes []struct {
		Name      string            `json:"Name"`
		Labels    map[string]string `json:"Labels"`
		UsageData struct {
			Size     int64 `json:"Size"`
			RefCount int64 `json:"RefCount"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		ID     string `json:"ID"`
		Size   int64  `json:"Size"`
		InUse  bool   `json:"InUse"`
		Shared bool   `json:"Shared"`
	} `json:"BuildCache"`
}

// DiskUsage reports space used by images, containers, volumes and the build
// cache. It can take a few seconds on hosts with many volumes.
func (c *Client) DiskUsage(ctx context.Context) (DiskUsage, error) {
	var du DiskUsage
	err := c.do(ctx, http.MethodGet, "/system/df", nil, nil, &du)
	return du, err
}

// BuildPrune removes unused build cache and returns the bytes reclaimed.
func (c *Client) BuildPrune(ctx context.Context) (int64, error) {
	var out struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	err := c.do(ctx, http.MethodPost, "/build/prune", url.Values{"all": {"true"}}, nil, &out)
	return out.SpaceReclaimed, err
}
