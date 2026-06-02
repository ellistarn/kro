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

func newCryptoEnv(t *testing.T) *cel.Env {
	t.Helper()
	env, err := cel.NewEnv(celCryptoFunctions()...)
	if err != nil {
		t.Fatalf("failed to create CEL env: %v", err)
	}
	return env
}

func evalCryptoString(t *testing.T, expr string) (string, error) {
	t.Helper()
	env := newCryptoEnv(t)
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
		return "", err
	}
	if types.IsError(out) {
		return "", out.(*types.Err)
	}
	return out.Value().(string), nil
}

func mustEvalCryptoString(t *testing.T, expr string) string {
	t.Helper()
	s, err := evalCryptoString(t, expr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return s
}

func parsePKCS8Key(t *testing.T, pemStr string) any {
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

func parsePKIXPub(t *testing.T, pemStr string) any {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		t.Fatal("failed to decode public key PEM")
	}
	if block.Type != "PUBLIC KEY" {
		t.Fatalf("unexpected PEM type: %s", block.Type)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse PKIX public key: %v", err)
	}
	return key
}

func parseCryptoCSR(t *testing.T, pemStr string) *x509.CertificateRequest {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		t.Fatal("failed to decode CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse CSR: %v", err)
	}
	return csr
}

// Key generation tests

func TestRSAGenerateKey_2048(t *testing.T) {
	s := mustEvalCryptoString(t, `rsa.generateKey(2048)`)
	key := parsePKCS8Key(t, s)
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("expected *rsa.PrivateKey, got %T", key)
	}
	if rsaKey.N.BitLen() != 2048 {
		t.Fatalf("expected 2048-bit, got %d", rsaKey.N.BitLen())
	}
}

func TestRSAGenerateKey_3072(t *testing.T) {
	s := mustEvalCryptoString(t, `rsa.generateKey(3072)`)
	key := parsePKCS8Key(t, s).(*rsa.PrivateKey)
	if key.N.BitLen() != 3072 {
		t.Fatalf("expected 3072-bit, got %d", key.N.BitLen())
	}
}

func TestRSAGenerateKey_4096(t *testing.T) {
	s := mustEvalCryptoString(t, `rsa.generateKey(4096)`)
	key := parsePKCS8Key(t, s).(*rsa.PrivateKey)
	if key.N.BitLen() != 4096 {
		t.Fatalf("expected 4096-bit, got %d", key.N.BitLen())
	}
}

func TestECDSAGenerateKey_P256(t *testing.T) {
	s := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	key := parsePKCS8Key(t, s)
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("expected *ecdsa.PrivateKey, got %T", key)
	}
	if ecKey.Curve != elliptic.P256() {
		t.Fatal("expected P-256 curve")
	}
}

func TestECDSAGenerateKey_P384(t *testing.T) {
	s := mustEvalCryptoString(t, `ecdsa.generateKey("P384")`)
	key := parsePKCS8Key(t, s).(*ecdsa.PrivateKey)
	if key.Curve != elliptic.P384() {
		t.Fatal("expected P-384 curve")
	}
}

func TestECDSAGenerateKey_P521(t *testing.T) {
	s := mustEvalCryptoString(t, `ecdsa.generateKey("P521")`)
	key := parsePKCS8Key(t, s).(*ecdsa.PrivateKey)
	if key.Curve != elliptic.P521() {
		t.Fatal("expected P-521 curve")
	}
}

func TestEd25519GenerateKey(t *testing.T) {
	s := mustEvalCryptoString(t, `ed25519.generateKey()`)
	key := parsePKCS8Key(t, s)
	_, ok := key.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("expected ed25519.PrivateKey, got %T", key)
	}
}

// Public key extraction tests

func TestCryptoPublicKey_RSA(t *testing.T) {
	priv := mustEvalCryptoString(t, `rsa.generateKey(2048)`)
	pub := mustEvalCryptoString(t, `crypto.publicKey("`+escapeCEL(priv)+`")`)
	key := parsePKIXPub(t, pub)
	if _, ok := key.(*rsa.PublicKey); !ok {
		t.Fatalf("expected *rsa.PublicKey, got %T", key)
	}
}

func TestCryptoPublicKey_ECDSA(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P384")`)
	pub := mustEvalCryptoString(t, `crypto.publicKey("`+escapeCEL(priv)+`")`)
	key := parsePKIXPub(t, pub)
	ecPub, ok := key.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("expected *ecdsa.PublicKey, got %T", key)
	}
	if ecPub.Curve != elliptic.P384() {
		t.Fatal("expected P-384 curve")
	}
}

func TestCryptoPublicKey_Ed25519(t *testing.T) {
	priv := mustEvalCryptoString(t, `ed25519.generateKey()`)
	pub := mustEvalCryptoString(t, `crypto.publicKey("`+escapeCEL(priv)+`")`)
	key := parsePKIXPub(t, pub)
	if _, ok := key.(ed25519.PublicKey); !ok {
		t.Fatalf("expected ed25519.PublicKey, got %T", key)
	}
}

func TestCryptoPublicKey_InvalidPEM(t *testing.T) {
	_, err := evalCryptoString(t, `crypto.publicKey("not a pem")`)
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}

func TestCryptoPublicKey_WrongPEMType(t *testing.T) {
	// Feed a CSR PEM instead of a private key
	_, err := evalCryptoString(t, `crypto.publicKey("-----BEGIN CERTIFICATE REQUEST-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A\n-----END CERTIFICATE REQUEST-----\n")`)
	if err == nil {
		t.Fatal("expected error for wrong PEM type")
	}
}

// CSR creation tests

func TestX509CreateCSR_WithCN(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	expr := `x509.createCertificateRequest("` + escapeCEL(priv) + `", {"subject": {"commonName": "app.example.com"}, "dnsNames": ["extra.example.com"]})`
	csrPEM := mustEvalCryptoString(t, expr)
	csr := parseCryptoCSR(t, csrPEM)
	if csr.Subject.CommonName != "app.example.com" {
		t.Fatalf("expected CN app.example.com, got %s", csr.Subject.CommonName)
	}
	// CN should be auto-added to DNSNames
	found := false
	for _, d := range csr.DNSNames {
		if d == "app.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatal("CN should be in DNSNames")
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}
}

func TestX509CreateCSR_NoCN_WithDNS(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	expr := `x509.createCertificateRequest("` + escapeCEL(priv) + `", {"subject": {}, "dnsNames": ["svc.cluster.local"]})`
	csrPEM := mustEvalCryptoString(t, expr)
	csr := parseCryptoCSR(t, csrPEM)
	if csr.Subject.CommonName != "" {
		t.Fatalf("expected empty CN, got %s", csr.Subject.CommonName)
	}
	if len(csr.DNSNames) != 1 || csr.DNSNames[0] != "svc.cluster.local" {
		t.Fatalf("unexpected DNSNames: %v", csr.DNSNames)
	}
}

func TestX509CreateCSR_WithIPAddresses(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	expr := `x509.createCertificateRequest("` + escapeCEL(priv) + `", {"subject": {"commonName": "node"}, "ipAddresses": ["10.0.0.1", "::1"]})`
	csrPEM := mustEvalCryptoString(t, expr)
	csr := parseCryptoCSR(t, csrPEM)
	if len(csr.IPAddresses) != 2 {
		t.Fatalf("expected 2 IPs, got %d", len(csr.IPAddresses))
	}
}

func TestX509CreateCSR_WithOrganization(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	expr := `x509.createCertificateRequest("` + escapeCEL(priv) + `", {"subject": {"commonName": "app", "organization": ["team-a", "team-b"]}})`
	csrPEM := mustEvalCryptoString(t, expr)
	csr := parseCryptoCSR(t, csrPEM)
	if len(csr.Subject.Organization) != 2 {
		t.Fatalf("expected 2 orgs, got %v", csr.Subject.Organization)
	}
}

func TestX509CreateCSR_InvalidKeyPEM(t *testing.T) {
	_, err := evalCryptoString(t, `x509.createCertificateRequest("bad", {"subject": {"commonName": "x"}})`)
	if err == nil {
		t.Fatal("expected error for invalid key PEM")
	}
}

func TestX509CreateCSR_MissingSubject(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	_, err := evalCryptoString(t, `x509.createCertificateRequest("`+escapeCEL(priv)+`", {"dnsNames": ["x"]})`)
	if err == nil {
		t.Fatal("expected error for missing subject")
	}
}

func TestX509CreateCSR_NoIdentity(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	_, err := evalCryptoString(t, `x509.createCertificateRequest("`+escapeCEL(priv)+`", {"subject": {}})`)
	if err == nil {
		t.Fatal("expected error when no identity provided")
	}
}

func TestX509CreateCSR_InvalidIP(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	_, err := evalCryptoString(t, `x509.createCertificateRequest("`+escapeCEL(priv)+`", {"subject": {"commonName": "x"}, "ipAddresses": ["not.an.ip"]})`)
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}
}

func TestX509CreateCSR_UnknownTemplateKey(t *testing.T) {
	priv := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	_, err := evalCryptoString(t, `x509.createCertificateRequest("`+escapeCEL(priv)+`", {"subject": {"commonName": "x"}, "bogus": "val"})`)
	if err == nil {
		t.Fatal("expected error for unknown template key")
	}
}

// Validation error tests

func TestRSAGenerateKey_InvalidSize(t *testing.T) {
	_, err := evalCryptoString(t, `rsa.generateKey(1024)`)
	if err == nil {
		t.Fatal("expected error for invalid RSA size")
	}
}

func TestECDSAGenerateKey_InvalidCurve(t *testing.T) {
	_, err := evalCryptoString(t, `ecdsa.generateKey("secp256k1")`)
	if err == nil {
		t.Fatal("expected error for invalid curve")
	}
}

// Freshness test

func TestKeyGenFreshness(t *testing.T) {
	k1 := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	k2 := mustEvalCryptoString(t, `ecdsa.generateKey("P256")`)
	if k1 == k2 {
		t.Fatal("two calls should produce different keys")
	}
}

// helper to escape PEM for embedding in CEL string literal
func escapeCEL(s string) string {
	out := ""
	for _, c := range s {
		switch c {
		case '\n':
			out += `\n`
		case '"':
			out += `\"`
		case '\\':
			out += `\\`
		default:
			out += string(c)
		}
	}
	return out
}
