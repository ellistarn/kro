package compiler

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
)

func celCertificateFunction() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("certificate.generateParams",
			cel.Overload("certificate_generateParams_map",
				[]*cel.Type{cel.DynType},
				cel.DynType,
				cel.UnaryBinding(func(arg ref.Val) ref.Val {
					return generateCertParams(arg)
				}),
			),
		),
	}
}

func generateCertParams(configVal ref.Val) ref.Val {
	raw, err := conversion.GoNativeType(configVal)
	if err != nil {
		return types.NewErr("certificate.generateParams: failed to convert config: %v", err)
	}
	config, ok := raw.(map[string]any)
	if !ok {
		return types.NewErr("certificate.generateParams: config must be a map, got %T", raw)
	}

	// Validate no unknown keys
	known := map[string]bool{"commonName": true, "algorithm": true, "size": true, "dnsNames": true}
	for k := range config {
		if !known[k] {
			return types.NewErr("certificate.generateParams: unknown config key %q; accepted keys: commonName, algorithm, size, dnsNames", k)
		}
	}

	// Required fields
	cn, err := requireString(config, "commonName")
	if err != nil {
		return types.NewErr("certificate.generateParams: %v", err)
	}
	if cn == "" {
		return types.NewErr("certificate.generateParams: commonName must not be empty")
	}
	algorithm, err := requireString(config, "algorithm")
	if err != nil {
		return types.NewErr("certificate.generateParams: %v", err)
	}
	size, err := requireInt(config, "size")
	if err != nil {
		return types.NewErr("certificate.generateParams: %v", err)
	}

	// Validate algorithm/size
	if err := validateAlgorithmSize(algorithm, size); err != nil {
		return types.NewErr("certificate.generateParams: %v", err)
	}

	// Optional dnsNames
	var dnsNames []string
	if v, exists := config["dnsNames"]; exists {
		list, ok := v.([]any)
		if !ok {
			return types.NewErr("certificate.generateParams: dnsNames must be a list, got %T", v)
		}
		for i, item := range list {
			s, ok := item.(string)
			if !ok {
				return types.NewErr("certificate.generateParams: dnsNames[%d] must be a string, got %T", i, item)
			}
			dnsNames = append(dnsNames, s)
		}
	}

	// Generate key
	priv, err := generateKey(algorithm, size)
	if err != nil {
		return types.NewErr("certificate.generateParams: %v", err)
	}

	// Build CSR
	sans := dedup(append([]string{cn}, dnsNames...))
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
		DNSNames: sans,
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, tmpl, priv)
	if err != nil {
		return types.NewErr("certificate.generateParams: failed to create CSR: %v", err)
	}

	// PEM encode
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return types.NewErr("certificate.generateParams: failed to marshal private key: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	return types.DefaultTypeAdapter.NativeToValue(map[string]any{
		"privateKey": string(privPEM),
		"csr":        string(csrPEM),
	})
}

func validateAlgorithmSize(algorithm string, size int) error {
	switch algorithm {
	case "RSA":
		if size != 2048 && size != 3072 && size != 4096 {
			return fmt.Errorf("invalid size %d for RSA; allowed: 2048, 3072, 4096", size)
		}
	case "ECDSA":
		if size != 256 && size != 384 {
			return fmt.Errorf("invalid size %d for ECDSA; allowed: 256, 384", size)
		}
	case "Ed25519":
		if size != 256 {
			return fmt.Errorf("invalid size %d for Ed25519; allowed: 256", size)
		}
	default:
		return fmt.Errorf("unsupported algorithm %q; allowed: RSA, ECDSA, Ed25519", algorithm)
	}
	return nil
}

func generateKey(algorithm string, size int) (any, error) {
	switch algorithm {
	case "RSA":
		return rsa.GenerateKey(rand.Reader, size)
	case "ECDSA":
		var curve elliptic.Curve
		if size == 256 {
			curve = elliptic.P256()
		} else {
			curve = elliptic.P384()
		}
		return ecdsa.GenerateKey(curve, rand.Reader)
	case "Ed25519":
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		return priv, err
	}
	return nil, fmt.Errorf("unsupported algorithm %q", algorithm)
}

func requireString(config map[string]any, key string) (string, error) {
	v, exists := config[key]
	if !exists {
		return "", fmt.Errorf("missing required field %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("field %q must be a string, got %T", key, v)
	}
	return s, nil
}

func requireInt(config map[string]any, key string) (int, error) {
	v, exists := config[key]
	if !exists {
		return 0, fmt.Errorf("missing required field %q", key)
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	}
	return 0, fmt.Errorf("field %q must be an integer, got %T", key, v)
}

func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		lower := strings.ToLower(s)
		if !seen[lower] {
			seen[lower] = true
			out = append(out, s)
		}
	}
	return out
}
