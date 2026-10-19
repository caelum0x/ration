// Package grpcserver implements webhook_ingestion.proto's
// WebhookIngestion service: StreamEvents subscribes a caller (in
// practice, services/gateway merging this into its own activity feed)
// to internal/eventbus's Bus.
package grpcserver

import (
	webhookingestionpb "github.com/caelum0x/ration/pkg/pb/webhookingestion"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/eventbus"
)

// Server implements webhookingestionpb.WebhookIngestionServer.
type Server struct {
	webhookingestionpb.UnimplementedWebhookIngestionServer

	bus *eventbus.Bus
}

// New constructs a Server backed by bus.
func New(bus *eventbus.Bus) *Server {
	return &Server{bus: bus}
}

// StreamEvents implements webhookingestionpb.WebhookIngestionServer.
func (s *Server) StreamEvents(req *webhookingestionpb.StreamEventsRequest, stream webhookingestionpb.WebhookIngestion_StreamEventsServer) error {
	ch, unsubscribe := s.bus.Subscribe(req.GetTenantId())
	defer unsubscribe()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&webhookingestionpb.NormalizedEvent{
				Id:       e.ID,
				TenantId: e.TenantID,
				Kind:     e.Kind,
				Summary:  e.Summary,
				AtUnix:   e.AtUnix,
			}); err != nil {
				return err
			}
		}
	}
}
