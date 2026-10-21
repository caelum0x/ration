package grpcserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	auditpb "github.com/caelum0x/ration/pkg/pb/audit"
	"github.com/caelum0x/ration/services/audit-log/internal/store"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "audit.log"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s)
}

func TestAppend_RejectsMissingSourceServiceOrRefID(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	if _, err := srv.Append(ctx, &auditpb.AppendRequest{RefId: "x"}); err == nil {
		t.Fatalf("expected error for missing source_service")
	}
	if _, err := srv.Append(ctx, &auditpb.AppendRequest{SourceService: "ledger-core"}); err == nil {
		t.Fatalf("expected error for missing ref_id")
	}
}

func TestAppend_ThenListRecords(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	resp, err := srv.Append(ctx, &auditpb.AppendRequest{
		SourceService:  "ledger-core",
		Kind:           auditpb.SourceKind_SOURCE_KIND_LEDGER_ENTRY,
		TenantId:       "t1",
		RefId:          "entry-1",
		PayloadJson:    `{"amount":100}`,
		OccurredAtUnix: 123,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if resp.GetRecord().GetSequence() != 1 {
		t.Fatalf("expected sequence 1, got %d", resp.GetRecord().GetSequence())
	}

	listResp, err := srv.ListRecords(ctx, &auditpb.ListRecordsRequest{TenantId: "t1"})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(listResp.GetRecords()) != 1 {
		t.Fatalf("expected 1 record, got %d", len(listResp.GetRecords()))
	}
	if listResp.GetRecords()[0].GetRefId() != "entry-1" {
		t.Fatalf("unexpected record: %+v", listResp.GetRecords()[0])
	}
}

func TestAppend_IsIdempotentOverGRPC(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	req := &auditpb.AppendRequest{SourceService: "ledger-core", Kind: auditpb.SourceKind_SOURCE_KIND_LEDGER_ENTRY, RefId: "entry-1", PayloadJson: "{}"}
	r1, err := srv.Append(ctx, req)
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	r2, err := srv.Append(ctx, req)
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if r1.GetRecord().GetSequence() != r2.GetRecord().GetSequence() {
		t.Fatalf("expected idempotent append to return the same record")
	}
}

func TestListRecords_Pagination(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := srv.Append(ctx, &auditpb.AppendRequest{
			SourceService: "ledger-core",
			Kind:          auditpb.SourceKind_SOURCE_KIND_LEDGER_ENTRY,
			RefId:         string(rune('a' + i)),
			PayloadJson:   "{}",
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	page1, err := srv.ListRecords(ctx, &auditpb.ListRecordsRequest{PageSize: 2})
	if err != nil {
		t.Fatalf("ListRecords page 1: %v", err)
	}
	if len(page1.GetRecords()) != 2 || page1.GetNextPageToken() == "" {
		t.Fatalf("expected a full page with a next_page_token, got %+v", page1)
	}

	page2, err := srv.ListRecords(ctx, &auditpb.ListRecordsRequest{PageSize: 2, PageToken: page1.GetNextPageToken()})
	if err != nil {
		t.Fatalf("ListRecords page 2: %v", err)
	}
	if len(page2.GetRecords()) != 1 || page2.GetNextPageToken() != "" {
		t.Fatalf("expected final partial page with no next token, got %+v", page2)
	}
}

func TestListRecords_RejectsInvalidPageToken(t *testing.T) {
	srv := newTestServer(t)
	if _, err := srv.ListRecords(context.Background(), &auditpb.ListRecordsRequest{PageToken: "not-a-number"}); err == nil {
		t.Fatalf("expected error for invalid page_token")
	}
}

func TestVerifyChain_IntactOnFreshStore(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := srv.Append(ctx, &auditpb.AppendRequest{SourceService: "ledger-core", RefId: string(rune('a' + i)), PayloadJson: "{}"}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	resp, err := srv.VerifyChain(ctx, &auditpb.VerifyChainRequest{})
	if err != nil {
		t.Fatalf("VerifyChain: %v", err)
	}
	if !resp.GetIntact() {
		t.Fatalf("expected chain to be intact, got %+v", resp)
	}
	if resp.GetRecordsChecked() != 3 {
		t.Fatalf("expected 3 records checked, got %d", resp.GetRecordsChecked())
	}
}

// issueTestLeafCert builds a throwaway self-signed certificate with the
// given CommonName. These tests construct a *peer.Peer directly (no
// real TLS handshake), so the cert only needs to be well-formed enough
// for x509.ParseCertificate/mtls.PeerCommonName to read its Subject —
// unlike pkg/mtls's own handshake tests, nothing here verifies a chain
// against a CA.
func issueTestLeafCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	return cert
}

// TestAppend_NoVerifiedPeer_TrustsRequestSourceService documents the
// unavoidable half of docs/THREAT-MODEL.md's "Known gaps" item 2: with
// no verified mTLS peer identity on the context (mTLS disabled, the
// default), Append has nothing more trustworthy than the request's own
// claimed source_service to record.
func TestAppend_NoVerifiedPeer_TrustsRequestSourceService(t *testing.T) {
	srv := newTestServer(t)
	resp, err := srv.Append(context.Background(), &auditpb.AppendRequest{
		SourceService: "ledger-core",
		RefId:         "ref-1",
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if got := resp.GetRecord().GetSourceService(); got != "ledger-core" {
		t.Errorf("SourceService = %q, want %q (request field, no verified peer)", got, "ledger-core")
	}
}

// TestAppend_VerifiedPeer_OverridesClaimedSourceService is the actual
// fix: a caller presenting a verified mTLS certificate for "agent" who
// claims source_service: "ledger-core" (forging the audit trail) gets
// overridden by the transport-verified identity, not trusted.
func TestAppend_VerifiedPeer_OverridesClaimedSourceService(t *testing.T) {
	srv := newTestServer(t)

	leafCert := issueTestLeafCert(t, "agent")
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leafCert}},
		},
	})

	resp, err := srv.Append(ctx, &auditpb.AppendRequest{
		SourceService: "ledger-core", // forged: the real caller is "agent"
		RefId:         "ref-1",
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if got := resp.GetRecord().GetSourceService(); got != "agent" {
		t.Errorf("SourceService = %q, want %q (verified peer identity, not the claimed field)", got, "agent")
	}
}
