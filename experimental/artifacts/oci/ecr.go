package oci

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// ECRAuth implements AuthProvider for Amazon ECR using a static token.
//
// Obtain a token via:
//
//	aws ecr get-login-password --region <region>
//
// ECR tokens are valid for 12 hours. When the token expires, create a new
// ECRAuth with a fresh token.
type ECRAuth struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewECRAuth creates an ECR auth provider with a pre-fetched token.
// The token should be the raw password from `aws ecr get-login-password`.
func NewECRAuth(token string) *ECRAuth {
	return &ECRAuth{
		token:  token,
		expiry: time.Now().Add(12 * time.Hour),
	}
}

// Authorization returns the Basic auth header for ECR.
// ECR uses "AWS" as the username with the token as the password.
func (e *ECRAuth) Authorization(_ context.Context, _ string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if time.Now().After(e.expiry) {
		return "", fmt.Errorf("ECR token expired; obtain a new one via `aws ecr get-login-password`")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("AWS:" + e.token))
	return "Basic " + encoded, nil
}
