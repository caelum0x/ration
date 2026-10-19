package servicecertca

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"
)

func generateTestCSR(t *testing.T, commonName string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	template := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: commonName},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		t.Fatalf("create test CSR: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestGenerateSelfSigned_ProducesUsableCA(t *testing.T) {
	ca, err := GenerateSelfSigned()
	if err != nil {
		t.Fatalf("GenerateSelfSigned() error = %v", err)
	}
	if len(ca.CAPEM()) == 0 {
		t.Fatal("CAPEM() is empty")
	}
}

func TestSignCSR_IssuesValidCertificate(t *testing.T) {
	ca, err := GenerateSelfSigned()
	if err != nil {
		t.Fatalf("GenerateSelfSigned() error = %v", err)
	}

	csrPEM := generateTestCSR(t, "card-issuance")
	certPEM, expiresAt, err := ca.SignCSR(csrPEM, "card-issuance", time.Hour)
	if err != nil {
		t.Fatalf("SignCSR() error = %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("expiresAt = %v, want in the future", expiresAt)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("SignCSR() did not return a PEM block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued leaf certificate: %v", err)
	}
	if leaf.Subject.CommonName != "card-issuance" {
		t.Errorf("CommonName = %q, want card-issuance", leaf.Subject.CommonName)
	}

	caBlock, _ := pem.Decode(ca.CAPEM())
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("issued leaf certificate does not verify against its own CA: %v", err)
	}
}

func TestSignCSR_RequiresServiceName(t *testing.T) {
	ca, _ := GenerateSelfSigned()
	csrPEM := generateTestCSR(t, "x")
	if _, _, err := ca.SignCSR(csrPEM, "", time.Hour); err == nil {
		t.Fatal("SignCSR() expected error for empty service_name, got nil")
	}
}

func TestSignCSR_RejectsMalformedCSR(t *testing.T) {
	ca, _ := GenerateSelfSigned()
	if _, _, err := ca.SignCSR([]byte("not a csr"), "service", time.Hour); err == nil {
		t.Fatal("SignCSR() expected error for malformed CSR, got nil")
	}
}

func TestSignCSR_ClampsTTL(t *testing.T) {
	ca, _ := GenerateSelfSigned()
	fixedNow := time.Unix(2_000_000_000, 0)
	ca.now = func() time.Time { return fixedNow }

	csrPEM := generateTestCSR(t, "svc")
	_, expiresAt, err := ca.SignCSR(csrPEM, "svc", 0)
	if err != nil {
		t.Fatalf("SignCSR() error = %v", err)
	}
	if !expiresAt.Equal(fixedNow.Add(DefaultTTL)) {
		t.Errorf("expiresAt = %v, want %v (DefaultTTL)", expiresAt, fixedNow.Add(DefaultTTL))
	}

	_, expiresAt, err = ca.SignCSR(csrPEM, "svc", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("SignCSR() error = %v", err)
	}
	if !expiresAt.Equal(fixedNow.Add(MaxTTL)) {
		t.Errorf("expiresAt = %v, want %v (clamped to MaxTTL)", expiresAt, fixedNow.Add(MaxTTL))
	}
}

func TestLoadFromPEM_RoundTrips(t *testing.T) {
	original, err := GenerateSelfSigned()
	if err != nil {
		t.Fatalf("GenerateSelfSigned() error = %v", err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(original.key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})

	loaded, err := LoadFromPEM(original.CAPEM(), keyPEM)
	if err != nil {
		t.Fatalf("LoadFromPEM() error = %v", err)
	}

	csrPEM := generateTestCSR(t, "reloaded-svc")
	certPEM, _, err := loaded.SignCSR(csrPEM, "reloaded-svc", time.Hour)
	if err != nil {
		t.Fatalf("SignCSR() on loaded CA error = %v", err)
	}
	if len(certPEM) == 0 {
		t.Fatal("SignCSR() on loaded CA returned empty cert")
	}
}
