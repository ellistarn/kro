package compiler

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
)

func newCertEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(celCertificateFunction()...)
	if err != nil {
		t.Fatalf("failed to create CEL env: %v", err)
	}
	return env
}

func evalCert(t *testing.T, expr string) (map[string]any, error) {
	t.Helper()
	env := newCertEnv(t)
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("compile error: %v", iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("program error: %v", err)
	}
	out, _, err := prg.Eval(cel.NoVars())
	if err != nil {
		return nil, err
	}
	if types.IsError(out) {
		return nil, out.(*types.Err)
	}
	native, err := out.ConvertToNative(nil)
	if err != nil {
		// Try map assertion directly
		m, ok := out.Value().(map[string]any)
		if !ok {
			t.Fatalf("unexpected output type: %T", out.Value())
		}
		return m, nil
	}
	m, ok := native.(map[string]any)
	if !ok {
		t.Fatalf("unexpected native type: %T", native)
	}
	return m, nil
}

func evalCertRef(t *testing.T, expr string) (map[string]any, error) {
	t.Helper()
	env := newCertEnv(t)
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("compile error: %v", iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("program error: %v", err)
	}
	out, _, err := prg.Eval(cel.NoVars())
	if err != nil {
		return nil, err
	}
	if types.IsError(out) {
		return nil, out.(*types.Err)
	}
	// Extract via Value()
	v := out.Value()
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	// ref.Val map - iterate
	m := map[string]any{}
	if mv, ok := out.(interface{ Value() any }); ok {
		if mm, ok := mv.Value().(map[string]any); ok {
			return mm, nil
		}
	}
	t.Fatalf("cannot extract map from %T", out)
	return m, nil
}

func mustEvalCert(t *testing.T, expr string) map[string]any {
	t.Helper()
	m, err := evalCertRef(t, expr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return m
}

func parsePrivateKey(t *testing.T, pemStr string) any {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		t.Fatal("failed to decode PEM")
	}
	if block.Type != "PRIVATE KEY" {
		t.Fatalf("unexpected PEM type: %s", block.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse PKCS8: %v", err)
	}
	return key
}

func parseCSR(t *testing.T, pemStr string) *x509.CertificateRequest {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		t.Fatal("failed to decode CSR PEM")
	}
	if block.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("unexpected PEM type: %s", block.Type)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse CSR: %v", err)
	}
	return csr
}

func TestCertificateGenerateParams_RSA2048(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "RSA", "size": 2048})`)
	key := parsePrivateKey(t, m["privateKey"].(string))
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("expected *rsa.PrivateKey, got %T", key)
	}
	if rsaKey.N.BitLen() != 2048 {
		t.Fatalf("expected 2048-bit key, got %d", rsaKey.N.BitLen())
	}
	csr := parseCSR(t, m["csr"].(string))
	if csr.Subject.CommonName != "test.example.com" {
		t.Fatalf("expected CN test.example.com, got %s", csr.Subject.CommonName)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}
}

func TestCertificateGenerateParams_RSA3072(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "RSA", "size": 3072})`)
	key := parsePrivateKey(t, m["privateKey"].(string))
	rsaKey := key.(*rsa.PrivateKey)
	if rsaKey.N.BitLen() != 3072 {
		t.Fatalf("expected 3072-bit key, got %d", rsaKey.N.BitLen())
	}
}

func TestCertificateGenerateParams_RSA4096(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "RSA", "size": 4096})`)
	key := parsePrivateKey(t, m["privateKey"].(string))
	rsaKey := key.(*rsa.PrivateKey)
	if rsaKey.N.BitLen() != 4096 {
		t.Fatalf("expected 4096-bit key, got %d", rsaKey.N.BitLen())
	}
}

func TestCertificateGenerateParams_ECDSAP256(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "ECDSA", "size": 256})`)
	key := parsePrivateKey(t, m["privateKey"].(string))
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("expected *ecdsa.PrivateKey, got %T", key)
	}
	if ecKey.Curve != elliptic.P256() {
		t.Fatal("expected P-256 curve")
	}
	csr := parseCSR(t, m["csr"].(string))
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}
}

func TestCertificateGenerateParams_ECDSAP384(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "ECDSA", "size": 384})`)
	key := parsePrivateKey(t, m["privateKey"].(string))
	ecKey := key.(*ecdsa.PrivateKey)
	if ecKey.Curve != elliptic.P384() {
		t.Fatal("expected P-384 curve")
	}
}

func TestCertificateGenerateParams_Ed25519(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "Ed25519", "size": 256})`)
	key := parsePrivateKey(t, m["privateKey"].(string))
	_, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("expected ed25519.PrivateKey, got %T", key)
	}
	csr := parseCSR(t, m["csr"].(string))
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}
}

func TestCertificateGenerateParams_DNSNames(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "app.example.com", "algorithm": "ECDSA", "size": 256, "dnsNames": ["extra.example.com", "other.example.com"]})`)
	csr := parseCSR(t, m["csr"].(string))
	expected := map[string]bool{"app.example.com": false, "extra.example.com": false, "other.example.com": false}
	for _, dns := range csr.DNSNames {
		if _, ok := expected[dns]; ok {
			expected[dns] = true
		}
	}
	for name, found := range expected {
		if !found {
			t.Errorf("expected DNS name %q in SAN, not found", name)
		}
	}
}

func TestCertificateGenerateParams_DNSNamesDedup(t *testing.T) {
	m := mustEvalCert(t, `certificate.generateParams({"commonName": "app.example.com", "algorithm": "ECDSA", "size": 256, "dnsNames": ["app.example.com"]})`)
	csr := parseCSR(t, m["csr"].(string))
	count := 0
	for _, dns := range csr.DNSNames {
		if dns == "app.example.com" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected commonName in SAN exactly once, got %d", count)
	}
}

func TestCertificateGenerateParams_TwoCallsDifferentKeys(t *testing.T) {
	m1 := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "ECDSA", "size": 256})`)
	m2 := mustEvalCert(t, `certificate.generateParams({"commonName": "test.example.com", "algorithm": "ECDSA", "size": 256})`)
	if m1["privateKey"] == m2["privateKey"] {
		t.Fatal("two calls should produce different keys")
	}
}

// Error cases

func TestCertificateGenerateParams_MissingCommonName(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"algorithm": "RSA", "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for missing commonName")
	}
}

func TestCertificateGenerateParams_MissingAlgorithm(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for missing algorithm")
	}
}

func TestCertificateGenerateParams_MissingSize(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "RSA"})`)
	if err == nil {
		t.Fatal("expected error for missing size")
	}
}

func TestCertificateGenerateParams_EmptyCommonName(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "", "algorithm": "RSA", "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for empty commonName")
	}
}

func TestCertificateGenerateParams_InvalidAlgorithm(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "DSA", "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for invalid algorithm")
	}
}

func TestCertificateGenerateParams_InvalidAlgorithmCase(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "rsa", "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for lowercase algorithm")
	}
}

func TestCertificateGenerateParams_InvalidSize(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "RSA", "size": 1024})`)
	if err == nil {
		t.Fatal("expected error for invalid RSA size")
	}
}

func TestCertificateGenerateParams_ECDSAInvalidSize(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "ECDSA", "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for invalid ECDSA size")
	}
}

func TestCertificateGenerateParams_Ed25519InvalidSize(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "Ed25519", "size": 512})`)
	if err == nil {
		t.Fatal("expected error for invalid Ed25519 size")
	}
}

func TestCertificateGenerateParams_UnknownConfigKey(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "RSA", "size": 2048, "keyType": "bad"})`)
	if err == nil {
		t.Fatal("expected error for unknown config key")
	}
}

func TestCertificateGenerateParams_WrongTypeCommonName(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": 123, "algorithm": "RSA", "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for non-string commonName")
	}
}

func TestCertificateGenerateParams_WrongTypeAlgorithm(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": 123, "size": 2048})`)
	if err == nil {
		t.Fatal("expected error for non-string algorithm")
	}
}

func TestCertificateGenerateParams_WrongTypeSize(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "RSA", "size": "big"})`)
	if err == nil {
		t.Fatal("expected error for non-int size")
	}
}

func TestCertificateGenerateParams_DNSNamesNonStringElement(t *testing.T) {
	_, err := evalCert(t, `certificate.generateParams({"commonName": "test", "algorithm": "ECDSA", "size": 256, "dnsNames": [123]})`)
	if err == nil {
		t.Fatal("expected error for non-string dnsNames element")
	}
}
