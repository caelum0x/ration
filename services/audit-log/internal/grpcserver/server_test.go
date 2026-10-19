package grpcserver

import (
	"context"
	"path/filepath"
	"testing"

	auditpb "github.com/caelum0x/ration/pkg/pb/audit"
	"github.com/caelum0x/ration/services/audit-log/internal/store"
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
