// Package grpcserver implements audit.proto's AuditLog service by
// delegating to internal/store. It is the one place that translates
// between wire types (pkg/pb/audit) and the storage layer's own
// SourceKind/Record types.
package grpcserver

import (
	"context"
	"strconv"
	"time"

	auditpb "github.com/caelum0x/ration/pkg/pb/audit"
	"github.com/caelum0x/ration/services/audit-log/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Server implements auditpb.AuditLogServer.
type Server struct {
	auditpb.UnimplementedAuditLogServer

	store *store.Store
	now   func() time.Time
}

// New wraps a *store.Store as an AuditLogServer.
func New(s *store.Store) *Server {
	return &Server{store: s, now: time.Now}
}

func toStoreKind(k auditpb.SourceKind) store.SourceKind {
	return store.SourceKind(k)
}

func toPBKind(k store.SourceKind) auditpb.SourceKind {
	return auditpb.SourceKind(k)
}

func toPBRecord(r store.Record) *auditpb.AuditRecord {
	return &auditpb.AuditRecord{
		Sequence:       r.Sequence,
		SourceService:  r.SourceService,
		Kind:           toPBKind(r.Kind),
		TenantId:       r.TenantID,
		RefId:          r.RefID,
		PayloadJson:    r.PayloadJSON,
		OccurredAtUnix: r.OccurredAtUnix,
		RecordedAtUnix: r.RecordedAtUnix,
		PrevHash:       r.PrevHash,
		Hash:           r.Hash,
	}
}

// Append implements auditpb.AuditLogServer.
func (s *Server) Append(ctx context.Context, req *auditpb.AppendRequest) (*auditpb.AppendResponse, error) {
	if req.GetSourceService() == "" {
		return nil, status.Error(codes.InvalidArgument, "source_service is required")
	}
	if req.GetRefId() == "" {
		return nil, status.Error(codes.InvalidArgument, "ref_id is required")
	}
	rec, err := s.store.Append(store.AppendInput{
		SourceService:  req.GetSourceService(),
		Kind:           toStoreKind(req.GetKind()),
		TenantID:       req.GetTenantId(),
		RefID:          req.GetRefId(),
		PayloadJSON:    req.GetPayloadJson(),
		OccurredAtUnix: req.GetOccurredAtUnix(),
	}, s.now().Unix())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audit-log: append: %v", err)
	}
	return &auditpb.AppendResponse{Record: toPBRecord(rec)}, nil
}

// ListRecords implements auditpb.AuditLogServer. page_token is the
// decimal sequence number to resume after (opaque to callers, but kept
// as a plain decimal string rather than anything encoded, since there
// is nothing here worth hiding and it keeps manual debugging easy).
func (s *Server) ListRecords(ctx context.Context, req *auditpb.ListRecordsRequest) (*auditpb.ListRecordsResponse, error) {
	afterSeq, err := parsePageToken(req.GetPageToken())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "audit-log: invalid page_token: %v", err)
	}
	records, next := s.store.List(req.GetTenantId(), afterSeq, int(req.GetPageSize()))
	resp := &auditpb.ListRecordsResponse{NextPageToken: formatPageToken(next)}
	for _, r := range records {
		resp.Records = append(resp.Records, toPBRecord(r))
	}
	return resp, nil
}

// VerifyChain implements auditpb.AuditLogServer.
func (s *Server) VerifyChain(ctx context.Context, req *auditpb.VerifyChainRequest) (*auditpb.VerifyChainResponse, error) {
	result := s.store.VerifyChain()
	return &auditpb.VerifyChainResponse{
		Intact:              result.Intact,
		RecordsChecked:      result.RecordsChecked,
		FirstBrokenSequence: result.FirstBrokenSequence,
		Detail:              result.Detail,
	}, nil
}

func parsePageToken(token string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	return strconv.ParseInt(token, 10, 64)
}

func formatPageToken(seq int64) string {
	if seq == 0 {
		return ""
	}
	return strconv.FormatInt(seq, 10)
}
