// Package server implements the aggregated API server for virtual Artifact
// resources. Artifacts are backed by OCI registries, not etcd — each GET
// decodes the base32 resource name back to an OCI URI, pulls (or cache-hits)
// the content, and returns it as a Kubernetes-shaped response.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kubernetes-sigs/kro/experimental/controller/compiler"
)

// Puller resolves and fetches OCI artifacts.
type Puller interface {
	Pull(ctx context.Context, uri string) (digest string, content map[string]any, err error)
}

// ArtifactStorage implements the HTTP handlers for virtual Artifacts.
type ArtifactStorage struct {
	puller Puller
	cache  *Cache
}

// NewArtifactStorage creates a new ArtifactStorage backed by the given puller
// and cache.
func NewArtifactStorage(puller Puller, cache *Cache) *ArtifactStorage {
	return &ArtifactStorage{
		puller: puller,
		cache:  cache,
	}
}

// Handler returns an http.Handler that routes artifact API requests.
func (s *ArtifactStorage) Handler() http.Handler {
	mux := http.NewServeMux()

	// API group discovery.
	mux.HandleFunc("/apis/artifacts.kro.run/v1alpha1", s.handleDiscovery)
	mux.HandleFunc("/apis/artifacts.kro.run/v1alpha1/", s.handleArtifacts)

	// Health check.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	return mux
}

// handleDiscovery returns the API resource list for the artifacts.kro.run/v1alpha1
// group version — required for kube-aggregator and client discovery.
func (s *ArtifactStorage) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": "artifacts.kro.run/v1alpha1",
		"resources": []map[string]any{
			{
				"name":       "artifacts",
				"singularName": "artifact",
				"namespaced": false,
				"kind":       "Artifact",
				"verbs":      []string{"get", "list"},
			},
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleArtifacts routes GET requests for individual artifacts and LIST
// requests for the collection.
func (s *ArtifactStorage) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the name from the path:
	//   /apis/artifacts.kro.run/v1alpha1/artifacts       -> LIST
	//   /apis/artifacts.kro.run/v1alpha1/artifacts/<name> -> GET
	path := strings.TrimPrefix(r.URL.Path, "/apis/artifacts.kro.run/v1alpha1/artifacts")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		s.handleList(w, r)
		return
	}
	s.handleGet(w, r, path)
}

// handleGet resolves a single artifact by its base32-encoded name.
func (s *ArtifactStorage) handleGet(w http.ResponseWriter, r *http.Request, name string) {
	uri, err := compiler.DecodeOCIName(name)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, fmt.Sprintf("invalid artifact name: %v", err))
		return
	}

	// Check cache first.
	if entry, ok := s.cache.Get(uri); ok {
		writeJSON(w, http.StatusOK, artifactObject(name, entry))
		return
	}

	// Pull from registry.
	digest, content, err := s.puller.Pull(r.Context(), uri)
	if err != nil {
		writeStatus(w, http.StatusBadGateway, fmt.Sprintf("pulling artifact %q: %v", uri, err))
		return
	}

	entry := &CacheEntry{
		URI:        uri,
		Digest:     digest,
		Content:    content,
		ResolvedAt: time.Now(),
	}
	s.cache.Put(entry)

	writeJSON(w, http.StatusOK, artifactObject(name, entry))
}

// handleList returns all cached artifacts.
func (s *ArtifactStorage) handleList(w http.ResponseWriter, _ *http.Request) {
	entries := s.cache.All()
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		name := compiler.EncodeOCIName(entry.URI)
		items = append(items, artifactObject(name, entry))
	}
	resp := map[string]any{
		"apiVersion": "artifacts.kro.run/v1alpha1",
		"kind":       "ArtifactList",
		"metadata":   map[string]any{"resourceVersion": ""},
		"items":      items,
	}
	writeJSON(w, http.StatusOK, resp)
}

// artifactObject builds the Kubernetes-shaped JSON response for one artifact.
func artifactObject(name string, entry *CacheEntry) map[string]any {
	return map[string]any{
		"apiVersion": "artifacts.kro.run/v1alpha1",
		"kind":       "Artifact",
		"metadata": map[string]any{
			"name": name,
			"annotations": map[string]string{
				"artifacts.kro.run/uri":    entry.URI,
				"artifacts.kro.run/digest": entry.Digest,
			},
			"creationTimestamp": entry.ResolvedAt.UTC().Format(time.RFC3339),
		},
		"status": map[string]any{
			"content": entry.Content,
		},
	}
}

// writeJSON marshals v and writes it as application/json.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeStatus writes a Kubernetes-style Status error response.
func writeStatus(w http.ResponseWriter, code int, message string) {
	resp := map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    message,
		"reason":     http.StatusText(code),
		"code":       code,
	}
	writeJSON(w, code, resp)
}
