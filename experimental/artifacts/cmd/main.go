// Binary entrypoint for the kro artifact server.
//
// The artifact server is an aggregated API server that serves virtual
// Artifact resources backed by OCI registries. Run with:
//
//	go run ./experimental/artifacts/cmd/ --ecr-token "$(aws ecr get-login-password --region us-west-2)"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/experimental/artifacts/oci"
	"github.com/kubernetes-sigs/kro/experimental/artifacts/server"
)

func main() {
	var (
		bindAddress string
		cacheSize   int
		ecrToken    string
	)

	flag.StringVar(&bindAddress, "bind-address", ":9443", "Address to bind the HTTPS server to")
	flag.IntVar(&cacheSize, "cache-size", 0, "Maximum number of cached artifacts (0 = unbounded)")
	flag.StringVar(&ecrToken, "ecr-token", "", "ECR auth token from `aws ecr get-login-password`")
	flag.Parse()

	if ecrToken == "" {
		ecrToken = os.Getenv("ECR_TOKEN")
	}

	// Build OCI client.
	var auth oci.AuthProvider
	if ecrToken != "" {
		auth = oci.NewECRAuth(ecrToken)
	}
	ociClient := oci.NewClient(auth)

	// Build server components.
	cache := server.NewCache(cacheSize)
	puller := &OCIPuller{client: ociClient}
	storage := server.NewArtifactStorage(puller, cache)

	handler := storage.Handler()
	mux := http.NewServeMux()
	// Mount the artifact handler.
	mux.Handle("/apis/artifacts.kro.run/", handler)
	mux.Handle("/healthz", handler)

	srv := &http.Server{
		Addr:    bindAddress,
		Handler: mux,
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("artifact-server listening on %s", bindAddress)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	srv.Shutdown(context.Background())
}

// OCIPuller wraps the OCI client to implement server.Puller.
type OCIPuller struct {
	client *oci.Client
}

// Pull resolves an OCI URI, fetches the artifact content, and deserializes
// it into a map.
func (p *OCIPuller) Pull(ctx context.Context, uri string) (string, map[string]any, error) {
	ref, err := oci.ParseReference(uri)
	if err != nil {
		return "", nil, fmt.Errorf("parsing reference: %w", err)
	}

	result, err := p.client.Pull(ctx, ref)
	if err != nil {
		return "", nil, fmt.Errorf("pulling %s: %w", uri, err)
	}

	// Deserialize YAML/JSON content into a map.
	content, err := deserialize(result.Content)
	if err != nil {
		return "", nil, fmt.Errorf("deserializing content from %s: %w", uri, err)
	}

	return result.Digest, content, nil
}

// deserialize attempts to parse raw bytes as YAML (which is a superset of
// JSON), falling back to JSON.
func deserialize(data []byte) (map[string]any, error) {
	// sigs.k8s.io/yaml handles both YAML and JSON.
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		// Fall back to pure JSON.
		if jsonErr := json.Unmarshal(data, &out); jsonErr != nil {
			return nil, fmt.Errorf("content is neither valid YAML nor JSON: yaml=%v, json=%v", err, jsonErr)
		}
	}
	return out, nil
}
