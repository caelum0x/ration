// Package webhookeventsclient is a typed gRPC client wrapper around the
// generated pb.WebhookIngestionClient, used for exactly one thing: the
// gateway (docs/SCALE-EXPANSION.md section 1) is the documented
// subscriber of services/webhook-ingestion's StreamEvents RPC, merging
// every normalized webhook event (e.g. a deposit received) into its own
// activity feed (internal/activity's Bus) so StreamActivity subscribers
// (the TUI) see webhook-driven events alongside card/bridge/deposit
// events the gateway publishes itself.
package webhookeventsclient

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	webhookingestionpb "github.com/caelum0x/ration/pkg/pb/webhookingestion"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client wraps a WebhookIngestionClient gRPC stub.
type Client struct {
	conn *grpc.ClientConn
	stub webhookingestionpb.WebhookIngestionClient
}

// Dial connects to webhook-ingestion at addr.
//
// extraOpts are applied after a plaintext-insecure default transport
// credential, so a caller passing its own grpc.WithTransportCredentials
// (e.g. pkg/mtls's Config.DialOption(), for mTLS to webhook-ingestion)
// simply overrides the default. Any other DialOption (interceptors,
// telemetry, …) is applied as given.
func Dial(addr string, extraOpts ...grpc.DialOption) (*Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("webhookeventsclient: addr must not be empty")
	}
	opts := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, extraOpts...)
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("webhookeventsclient: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, stub: webhookingestionpb.NewWebhookIngestionClient(conn)}, nil
}

// NewFromConn wraps an already-established connection — used by tests
// (bufconn) and anywhere the caller wants to manage the ClientConn
// lifecycle itself.
func NewFromConn(conn *grpc.ClientConn) *Client {
	return &Client{conn: conn, stub: webhookingestionpb.NewWebhookIngestionClient(conn)}
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// retryBackoff is how long RelayInto waits before re-subscribing after
// the stream ends (EOF, webhook-ingestion restarting, a transient
// network blip) — a fixed, modest backoff is enough here: this is a
// best-effort activity-feed merge, not a correctness-critical delivery
// path (every webhook event webhook-ingestion processes already called
// ledger-core directly; this stream only mirrors a human-readable
// summary of that into the TUI's live feed).
const retryBackoff = 2 * time.Second

// RelayInto subscribes to webhook-ingestion's StreamEvents and republishes
// every event onto bus until ctx is cancelled, automatically
// re-subscribing (after retryBackoff) if the stream ends for any other
// reason. Intended to run in its own goroutine for the lifetime of the
// gateway process.
func (c *Client) RelayInto(ctx context.Context, bus *activity.Bus, log *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.relayOnce(ctx, bus); err != nil && ctx.Err() == nil {
			log.Warn("webhookeventsclient: stream ended, retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryBackoff):
		}
	}
}

func (c *Client) relayOnce(ctx context.Context, bus *activity.Bus) error {
	stream, err := c.stub.StreamEvents(ctx, &webhookingestionpb.StreamEventsRequest{})
	if err != nil {
		return fmt.Errorf("webhookeventsclient: subscribe: %w", err)
	}
	for {
		e, err := stream.Recv()
		if err != nil {
			return err
		}
		bus.Publish(activity.Event{
			ID:       e.GetId(),
			TenantID: e.GetTenantId(),
			Kind:     e.GetKind(),
			Summary:  e.GetSummary(),
			AtUnix:   e.GetAtUnix(),
		})
	}
}
