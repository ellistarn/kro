package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// AuthProvider returns an authorization header value for a given registry host.
type AuthProvider interface {
	Authorization(ctx context.Context, registry string) (string, error)
}

// Client pulls single-layer OCI artifacts from registries.
type Client struct {
	httpClient *http.Client
	auth       AuthProvider
}

// NewClient creates a client with the default http.Client.
func NewClient(auth AuthProvider) *Client {
	return &Client{
		httpClient: http.DefaultClient,
		auth:       auth,
	}
}

// PullResult contains the resolved content and metadata.
type PullResult struct {
	Digest  string // resolved digest (sha256:...)
	Content []byte // raw layer content (YAML or JSON)
}

// Pull resolves a reference and fetches the first layer's content.
func (c *Client) Pull(ctx context.Context, ref Reference) (*PullResult, error) {
	// Step 1: resolve the manifest to get the layer descriptor.
	manifest, err := c.fetchManifest(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest: %w", err)
	}
	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("manifest has no layers")
	}
	layer := manifest.Layers[0]

	// Step 2: fetch the blob.
	content, err := c.fetchBlob(ctx, ref.Registry, ref.Repository, layer.Digest)
	if err != nil {
		return nil, fmt.Errorf("fetching blob: %w", err)
	}
	return &PullResult{Digest: layer.Digest, Content: content}, nil
}

// ociManifest is the minimal subset of an OCI/Docker manifest we need.
type ociManifest struct {
	Layers []descriptor `json:"layers"`
}

type descriptor struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func (c *Client) fetchManifest(ctx context.Context, ref Reference) (*ociManifest, error) {
	// Use digest directly if available, otherwise use tag.
	reference := ref.Tag
	if ref.Digest != "" {
		reference = ref.Digest
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, reference)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")

	resp, err := c.do(ctx, req, ref.Registry)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var m ociManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}
	return &m, nil
}

func (c *Client) fetchBlob(ctx context.Context, registry, repository, digest string) ([]byte, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", registry, repository, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.do(ctx, req, registry)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	return io.ReadAll(resp.Body)
}

// do executes an HTTP request with authorization.
func (c *Client) do(ctx context.Context, req *http.Request, registry string) (*http.Response, error) {
	if c.auth != nil {
		authz, err := c.auth.Authorization(ctx, registry)
		if err != nil {
			return nil, fmt.Errorf("getting authorization: %w", err)
		}
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
	}
	return c.httpClient.Do(req)
}

// checkResponse returns a descriptive error for non-2xx responses.
func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("authentication required (401): %s", body)
	case http.StatusNotFound:
		return fmt.Errorf("not found (404): %s", body)
	default:
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, body)
	}
}
