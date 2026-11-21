package docker

import (
	"context"
	"net/http"
	"net/url"
)

// The updater recreates Docklet's own container from its inspect output, so
// it works with Docker's JSON as-is instead of the typed ContainerSpec.

// ContainerInspectRaw returns the inspect document.
func (c *Client) ContainerInspectRaw(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
	return out, err
}

// ContainerCreateRaw creates a container from a create body.
func (c *Client) ContainerCreateRaw(ctx context.Context, name string, body map[string]any) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out)
	return out.ID, err
}

// ContainerRename renames a container.
func (c *Client) ContainerRename(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// NetworkConnect connects a container to a network with endpoint settings.
func (c *Client) NetworkConnect(ctx context.Context, network, container string, endpoint map[string]any) error {
	return c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil,
		map[string]any{"Container": container, "EndpointConfig": endpoint}, nil)
}

// ImageInspectRaw returns an image's inspect document.
func (c *Client) ImageInspectRaw(ctx context.Context, ref string) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &out)
	return out, err
}
