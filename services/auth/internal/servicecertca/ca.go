// Package servicecertca implements auth.proto's IssueServiceCert: a
// runtime-issuance CA that signs a service's certificate signing
// request (CSR), returning a short-lived leaf certificate the service
// can present for mTLS (see pkg/mtls, infra/dev-ca/).
//
// This is new capability (docs/SCALE-EXPANSION.md section 1,
// docs/adr/0004), and a deliberately different CA from
// infra/dev-ca/generate-dev-ca.sh's: that script issues static,
// offline, 1-year certs once per service, by hand, outside any running
// process. This CA signs on demand, at runtime, from inside the auth
// service — the mechanism a real deployment would build on to let a
// service request (or be issued, via an operator-driven rotation) a
// fresh short-lived cert without a redeploy, and to let a compromised
// service's identity be left to expire (or, with a future CRL/OCSP
// addition, revoked) without touching every other service's identity.
//
// TODO(production): this package's own CA private key is either
// generated fresh in memory (ephemeral — every cert it ever issued stops
// being trusted, from every verifier's point of view, as soon as this
// CA's own root is regenerated on the next restart, since nothing trusts
// the new root yet) or loaded from a PEM file on local disk
// (AUTH_CA_CERT_FILE/AUTH_CA_KEY_FILE) with no HSM, no rotation and no
// distribution story beyond "put the file where this process can read
// it." A real deployment needs a managed PKI (AWS Private CA / ACM PCA,
// HashiCorp Vault PKI) backing this, exactly as
// infra/dev-ca/generate-dev-ca.sh's own header says for its CA.
package servicecertca

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// DefaultTTL is used when a caller requests ttl_seconds=0.
const DefaultTTL = 24 * time.Hour

// MaxTTL bounds how long-lived an issued leaf certificate may be.
const MaxTTL = 7 * 24 * time.Hour

// caKeyBits is the RSA key size for both the CA itself and (when
// generating one in GenerateSelfSigned) matches what a CSR's own key
// should be — CSRs may use any key size/algorithm x509 supports, this
// constant only governs keys this package generates.
const caKeyBits = 2048

// CA signs service certificate requests against one CA keypair.
type CA struct {
	mu  sync.Mutex
	key *rsa.PrivateKey
	// cert is the CA's own self-signed certificate.
	cert *x509.Certificate
	// certPEM is cert, pre-encoded, returned verbatim with every
	// IssueServiceCert response so a caller can build a trust pool.
	certPEM []byte

	nextSerial *big.Int
	now        func() time.Time
}

// GenerateSelfSigned creates a brand-new CA keypair and self-signed
// certificate in memory. See this package's doc comment for why that is
// ephemeral and not production-ready on its own.
func GenerateSelfSigned() (*CA, error) {
	key, err := rsa.GenerateKey(rand.Reader, caKeyBits)
	if err != nil {
		return nil, fmt.Errorf("servicecertca: generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Ration"},
			CommonName:   "Ration Auth Service CA - runtime issuance",
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("servicecertca: create self-signed CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("servicecertca: parse generated CA certificate: %w", err)
	}

	nextSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	return &CA{
		key:        key,
		cert:       cert,
		certPEM:    encodePEM("CERTIFICATE", certDER),
		nextSerial: nextSerial,
		now:        time.Now,
	}, nil
}

// LoadFromPEM constructs a CA from an existing certificate and RSA
// private key (both PEM-encoded) — for a deployment that provisions
// AUTH_CA_CERT_FILE/AUTH_CA_KEY_FILE from a real, persisted CA instead of
// letting this service generate an ephemeral one.
func LoadFromPEM(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("servicecertca: cert file does not contain a PEM CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("servicecertca: parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("servicecertca: key file does not contain a PEM block")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		key2, err2 := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("servicecertca: parse CA private key (tried PKCS1 and PKCS8): %w", err)
		}
		rsaKey, ok := key2.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("servicecertca: CA private key is not RSA")
		}
		key = rsaKey
	}

	nextSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	return &CA{
		key:        key,
		cert:       cert,
		certPEM:    certPEM,
		nextSerial: nextSerial,
		now:        time.Now,
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("servicecertca: generate serial number: %w", err)
	}
	return serial, nil
}

func encodePEM(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

// CAPEM returns this CA's own self-signed certificate, PEM-encoded.
func (ca *CA) CAPEM() []byte {
	return ca.certPEM
}

// SignCSR validates and signs csrPEM (a PEM-encoded PKCS#10 certificate
// signing request), issuing a leaf certificate for serviceName valid for
// ttl (clamped to (0, MaxTTL], defaulting to DefaultTTL when ttl <= 0).
func (ca *CA) SignCSR(csrPEM []byte, serviceName string, ttl time.Duration) (certPEM []byte, expiresAt time.Time, err error) {
	if serviceName == "" {
		return nil, time.Time{}, fmt.Errorf("servicecertca: service_name is required")
	}

	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, time.Time{}, fmt.Errorf("servicecertca: csr_pem does not contain a PEM CERTIFICATE REQUEST block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("servicecertca: parse CSR: %w", err)
	}
	// CheckSignature proves whoever submitted this CSR holds the private
	// key matching the public key it carries — without this, anyone
	// could submit an arbitrary public key and have this CA vouch for
	// it without the requester ever proving possession of the matching
	// private key.
	if err := csr.CheckSignature(); err != nil {
		return nil, time.Time{}, fmt.Errorf("servicecertca: CSR signature verification failed: %w", err)
	}

	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()

	serial := new(big.Int).Set(ca.nextSerial)
	ca.nextSerial.Add(ca.nextSerial, big.NewInt(1))

	now := ca.now()
	expiresAt = now.Add(ttl)
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Ration"}, CommonName: serviceName},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     expiresAt,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{serviceName, "localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("servicecertca: sign certificate for %s: %w", serviceName, err)
	}

	return encodePEM("CERTIFICATE", certDER), expiresAt, nil
}
