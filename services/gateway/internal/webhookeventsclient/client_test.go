package webhookeventsclient

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	webhookingestionpb "github.com/caelum0x/ration/pkg/pb/webhookingestion"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeWebhookIngestionServer streams a fixed set of events once per
// StreamEvents call, then blocks until the client cancels — enough to
// exercise RelayInto's publish loop over a real gRPC stream (via
// bufconn) without a real network socket.
type fakeWebhookIngestionServer struct {
	webhookingestionpb.UnimplementedWebhookIngestionServer
	events []*webhookingestionpb.NormalizedEvent
}

func (f *fakeWebhookIngestionServer) StreamEvents(req *webhookingestionpb.StreamEventsRequest, stream webhookingestionpb.WebhookIngestion_StreamEventsServer) error {
	for _, e := range f.events {
		if err := stream.Send(e); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func newTestClient(t *testing.T, server *fakeWebhookIngestionServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	webhookingestionpb.RegisterWebhookIngestionServer(grpcServer, server)
	go func() {
		_ = grpcServer.Serve(lis)
	}()
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return NewFromConn(conn)
}

func TestRelayInto_PublishesEventsAndStopsOnCancel(t *testing.T) {
	server := &fakeWebhookIngestionServer{
		events: []*webhookingestionpb.NormalizedEvent{
			{Id: "evt_1", TenantId: "tenant_1", Kind: "deposit", Summary: "deposit received", AtUnix: 1},
		},
	}
	client := newTestClient(t, server)

	bus := activity.NewBus()
	ch, unsubscribe := bus.Subscribe("tenant_1")
	defer unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		client.RelayInto(ctx, bus, log)
		close(done)
	}()

	select {
	case e := <-ch:
		if e.Kind != "deposit" || e.TenantID != "tenant_1" {
			t.Errorf("event = %+v, want kind=deposit tenant_id=tenant_1", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for relayed activity event")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RelayInto did not return after context cancellation")
	}
}

func TestDial_RejectsEmptyAddr(t *testing.T) {
	if _, err := Dial(""); err == nil {
		t.Fatal("Dial(\"\") expected error, got nil")
	}
}

func TestClient_Close_NilConnIsNoop(t *testing.T) {
	c := &Client{}
	if err := c.Close(); err != nil {
		t.Errorf("Close() on zero-value Client error = %v, want nil", err)
	}
}
