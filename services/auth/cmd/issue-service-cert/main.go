// Command issue-service-cert is a one-shot client for auth.proto's
// IssueServiceCert RPC (services/auth/internal/servicecertca): given a
// PEM-encoded CSR, it asks a running `auth` instance to sign a leaf
// certificate and writes the result to disk.
//
// This is the client half of docs/infra/dev-ca's cert-rotation story
// (scripts/rotate-dev-ca.sh is the orchestrator — see that script's
// own doc comment for the full picture): auth's own CA, when started
// with AUTH_CA_CERT_FILE/AUTH_CA_KEY_FILE pointing at
// infra/dev-ca/out/ca.{pem,key} (the same static dev CA every
// service's *_MTLS_CA_FILE already trusts — see
// infra/dev-ca/generate-dev-ca.sh), signs leaf certs against that
// exact trust root, so a rotated leaf validates everywhere with zero
// redistribution of a new CA to anything.
//
// Usage:
//
//	issue-service-cert \
//	  -auth-addr localhost:7010 \
//	  -service card-issuance \
//	  -csr /path/to/card-issuance.csr.pem \
//	  -ttl 720h \
//	  -out-cert /path/to/card-issuance-cert.pem
//
// Exits nonzero (with a message on stderr) on any dial, RPC, or file
// I/O failure — safe to call from a script that should abort the whole
// rotation run on a single service's failure (see
// scripts/rotate-dev-ca.sh's set -euo pipefail).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	authpb "github.com/caelum0x/ration/pkg/pb/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "issue-service-cert: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("issue-service-cert", flag.ContinueOnError)
	authAddr := fs.String("auth-addr", "localhost:7010", "auth gRPC address (plaintext — this tool is dev/rotation tooling, not a production client)")
	service := fs.String("service", "", "service name the issued cert's CN/SAN will carry, e.g. \"card-issuance\" (required)")
	csrPath := fs.String("csr", "", "path to a PEM-encoded PKCS#10 certificate signing request (required)")
	ttl := fs.Duration("ttl", 720*time.Hour, "requested certificate lifetime (server clamps to servicecertca.MaxTTL = 7 days if larger)")
	outCert := fs.String("out-cert", "", "path to write the signed certificate PEM to (required)")
	outCA := fs.String("out-ca", "", "optional path to also write the issuing CA's own certificate PEM to")
	timeout := fs.Duration("timeout", 10*time.Second, "RPC timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *service == "" {
		return fmt.Errorf("-service is required")
	}
	if *csrPath == "" {
		return fmt.Errorf("-csr is required")
	}
	if *outCert == "" {
		return fmt.Errorf("-out-cert is required")
	}

	csrPEM, err := os.ReadFile(*csrPath)
	if err != nil {
		return fmt.Errorf("read CSR file %s: %w", *csrPath, err)
	}

	conn, err := grpc.NewClient(*authAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial auth at %s: %w", *authAddr, err)
	}
	defer conn.Close()
	client := authpb.NewAuthClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	resp, err := client.IssueServiceCert(ctx, &authpb.IssueServiceCertRequest{
		ServiceName: *service,
		CsrPem:      csrPEM,
		TtlSeconds:  int64(ttl.Seconds()),
	})
	if err != nil {
		return fmt.Errorf("IssueServiceCert RPC for %s: %w", *service, err)
	}

	if err := os.WriteFile(*outCert, resp.GetCertPem(), 0o600); err != nil {
		return fmt.Errorf("write issued cert to %s: %w", *outCert, err)
	}
	if *outCA != "" {
		if err := os.WriteFile(*outCA, resp.GetCaPem(), 0o644); err != nil {
			return fmt.Errorf("write issuing CA cert to %s: %w", *outCA, err)
		}
	}

	expiresAt := time.Unix(resp.GetExpiresAtUnix(), 0)
	fmt.Fprintf(stdout, "issue-service-cert: issued %s cert, valid until %s, written to %s\n",
		*service, expiresAt.Format(time.RFC3339), *outCert)
	return nil
}
