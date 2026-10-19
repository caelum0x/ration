package mtls

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/health"
)

// --- test PKI helpers -------------------------------------------------
//
// These build a throwaway CA and leaf certs entirely in-memory (no
// shelling out to openssl, no filesystem fixtures checked into the
// repo) so the test suite proves the real TLS handshake behavior of
// this package end to end: a server requiring a client cert actually
// rejects a client that doesn't present one signed by the same CA, and
// accepts one that does.

type testCA struct {
	certPEM []byte
	cert    *x509.Certificate
	key     *rsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ration-dev-ca-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &testCA{certPEM: certPEM, cert: cert, key: key}
}

// issueLeaf signs a leaf certificate for the given DNS name, usable as
// either a server or client cert (both ExtKeyUsage bits set, which is
// fine for a test fixture).
func (ca *testCA) issueLeaf(t *testing.T, cn string, serial int64) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		DNSNames:     []string{cn, "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// writeFixture writes a PEM blob to dir/name and returns the path.
func writeFixture(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
	return path
}

// --- tests --------------------------------------------------------------

func TestLoadFromEnv_DisabledByDefault(t *testing.T) {
	t.Setenv("FOO_MTLS_ENABLED", "")
	cfg, err := LoadFromEnv("FOO")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Fatalf("expected Enabled=false when %s is unset", envKey("FOO", "ENABLED"))
	}
}

func TestLoadFromEnv_EnabledRequiresAllFiles(t *testing.T) {
	t.Setenv("FOO_MTLS_ENABLED", "true")
	t.Setenv("FOO_MTLS_CERT_FILE", "")
	t.Setenv("FOO_MTLS_KEY_FILE", "")
	t.Setenv("FOO_MTLS_CA_FILE", "")
	_, err := LoadFromEnv("FOO")
	if err == nil {
		t.Fatalf("expected an error when enabled but cert/key/ca files are missing")
	}
}

func TestLoadFromEnv_EnabledWithAllFiles(t *testing.T) {
	t.Setenv("FOO_MTLS_ENABLED", "true")
	t.Setenv("FOO_MTLS_CERT_FILE", "/tmp/cert.pem")
	t.Setenv("FOO_MTLS_KEY_FILE", "/tmp/key.pem")
	t.Setenv("FOO_MTLS_CA_FILE", "/tmp/ca.pem")
	cfg, err := LoadFromEnv("FOO")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled || cfg.CertFile != "/tmp/cert.pem" || cfg.KeyFile != "/tmp/key.pem" || cfg.CAFile != "/tmp/ca.pem" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestDisabledConfig_RoundTripsOverPlaintext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialAddr, stop := startHealthServer(t, Config{})
	defer stop()

	dialOpt, err := (Config{}).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	conn, err := grpc.NewClient(dialAddr, dialOpt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check over plaintext failed: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("unexpected status: %v", resp.Status)
	}
}

// TestMTLS_ClientWithValidCertSucceeds proves the real end-to-end
// handshake: a server configured with ServerOption() (requiring and
// verifying a client cert) accepts a client dialing with DialOption()
// that presents a cert signed by the same CA.
func TestMTLS_ClientWithValidCertSucceeds(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t)
	caPath := writeFixture(t, dir, "ca.pem", ca.certPEM)

	serverCertPEM, serverKeyPEM := ca.issueLeaf(t, "ledger-core-test", 2)
	serverCertPath := writeFixture(t, dir, "server-cert.pem", serverCertPEM)
	serverKeyPath := writeFixture(t, dir, "server-key.pem", serverKeyPEM)

	clientCertPEM, clientKeyPEM := ca.issueLeaf(t, "gateway-test", 3)
	clientCertPath := writeFixture(t, dir, "client-cert.pem", clientCertPEM)
	clientKeyPath := writeFixture(t, dir, "client-key.pem", clientKeyPEM)

	serverCfg := Config{Enabled: true, CertFile: serverCertPath, KeyFile: serverKeyPath, CAFile: caPath}
	addr, stop := startHealthServer(t, serverCfg)
	defer stop()

	clientCfg := Config{Enabled: true, CertFile: clientCertPath, KeyFile: clientKeyPath, CAFile: caPath, ServerName: "ledger-core-test"}
	dialOpt, err := clientCfg.DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	conn, err := grpc.NewClient(addr, dialOpt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("expected mTLS handshake to succeed, got: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("unexpected status: %v", resp.Status)
	}
}

// TestMTLS_ClientWithoutCertIsRejected proves the server actually
// enforces RequireAndVerifyClientCert: a client that dials in plaintext
// against an mTLS-only server must fail, never silently downgrade.
func TestMTLS_ClientWithoutCertIsRejected(t *testing.T) {
	dir := t.TempDir()
	ca := newTestCA(t)
	caPath := writeFixture(t, dir, "ca.pem", ca.certPEM)
	serverCertPEM, serverKeyPEM := ca.issueLeaf(t, "ledger-core-test", 2)
	serverCertPath := writeFixture(t, dir, "server-cert.pem", serverCertPEM)
	serverKeyPath := writeFixture(t, dir, "server-key.pem", serverKeyPEM)

	serverCfg := Config{Enabled: true, CertFile: serverCertPath, KeyFile: serverKeyPath, CAFile: caPath}
	addr, stop := startHealthServer(t, serverCfg)
	defer stop()

	// Dial in plaintext against an mTLS-only server.
	insecureDialOpt, err := (Config{}).DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	conn, err := grpc.NewClient(addr, insecureDialOpt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := healthpb.NewHealthClient(conn)
	_, err = client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatalf("expected plaintext dial against an mTLS-only server to fail")
	}
	if status.Code(err) == codes.OK {
		t.Fatalf("expected a transport-level failure, got OK")
	}
}

// TestMTLS_ClientWithUntrustedCAIsRejected proves the server rejects a
// client cert signed by a different CA than the one it trusts.
func TestMTLS_ClientWithUntrustedCAIsRejected(t *testing.T) {
	dir := t.TempDir()
	trustedCA := newTestCA(t)
	untrustedCA := newTestCA(t)

	trustedCAPath := writeFixture(t, dir, "trusted-ca.pem", trustedCA.certPEM)
	serverCertPEM, serverKeyPEM := trustedCA.issueLeaf(t, "ledger-core-test", 2)
	serverCertPath := writeFixture(t, dir, "server-cert.pem", serverCertPEM)
	serverKeyPath := writeFixture(t, dir, "server-key.pem", serverKeyPEM)

	serverCfg := Config{Enabled: true, CertFile: serverCertPath, KeyFile: serverKeyPath, CAFile: trustedCAPath}
	addr, stop := startHealthServer(t, serverCfg)
	defer stop()

	// Client presents a cert signed by the untrusted CA, but still
	// trusts (verifies the server against) the actually-trusted CA —
	// modeling a rogue/compromised service whose client cert was never
	// issued by the real internal CA.
	untrustedClientCertPEM, untrustedClientKeyPEM := untrustedCA.issueLeaf(t, "rogue-test", 9)
	untrustedClientCertPath := writeFixture(t, dir, "rogue-cert.pem", untrustedClientCertPEM)
	untrustedClientKeyPath := writeFixture(t, dir, "rogue-key.pem", untrustedClientKeyPEM)

	clientCfg := Config{
		Enabled:    true,
		CertFile:   untrustedClientCertPath,
		KeyFile:    untrustedClientKeyPath,
		CAFile:     trustedCAPath,
		ServerName: "ledger-core-test",
	}
	dialOpt, err := clientCfg.DialOption()
	if err != nil {
		t.Fatalf("DialOption: %v", err)
	}
	conn, err := grpc.NewClient(addr, dialOpt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := healthpb.NewHealthClient(conn)
	_, err = client.Check(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatalf("expected a client cert from an untrusted CA to be rejected")
	}
}

// startHealthServer starts a real gRPC health server on loopback using
// cfg's ServerOption(), returning its dial address and a stop func.
func startHealthServer(t *testing.T, cfg Config) (addr string, stop func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	opt, err := cfg.ServerOption()
	if err != nil {
		t.Fatalf("ServerOption: %v", err)
	}
	srv := grpc.NewServer(opt)
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(lis)
	}()

	return lis.Addr().String(), func() {
		srv.Stop()
		<-done
	}
}
