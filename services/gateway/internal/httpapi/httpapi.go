// Package httpapi is a REST/JSON facade in front of gateway.proto's
// Gateway gRPC service (docs/SCALE-EXPANSION.md section 7, docs/adr/0003).
//
// It is a hand-rolled translation layer, not a second implementation:
// every handler in this package does nothing but decode an HTTP
// request into the matching gatewaypb request message, call the same
// GatewayClient gRPC stub cmd/gateway/main.go dials back to its own
// gRPC listener, and encode the response — so a REST caller is, in
// every way that matters, calling the exact same Gateway RPC a gRPC
// caller (the TUI, the CLI, sdk/go/ration, sdk/rust/ration-client)
// would. As ADR 0003 puts it: "REST is a convenience facade over the
// gRPC source of truth," never a parallel implementation of the
// rationing math or card-issuance logic gateway.proto's RPCs front.
//
// Every route is namespaced under /v1 and maps directly onto one RPC:
//
//	POST   /v1/cards                                 IssueCard
//	GET    /v1/cards/{card_id}                       GetCard
//	PATCH  /v1/cards/{card_id}/limit                 AdjustCardLimit
//	GET    /v1/tenants/{tenant_id}/cards             ListCards
//	POST   /v1/tenants/{tenant_id}/bridge-funding    RationBridgeFunding
//	POST   /v1/tenants/{tenant_id}/deposits/simulate SimulateDeposit
//	GET    /v1/activity[?tenant_id=...]              StreamActivity (newline-delimited JSON)
//	GET    /v1/dashboard                             judge-facing live web dashboard (dashboard.go, static HTML/JS)
//	GET    /v1/dashboard/snapshot                    GetDashboardSnapshot
//	GET    /v1/dashboard/activity/stream             StreamActivity (text/event-stream, for dashboard.html's EventSource)
//
// This package only ever imports the generated gatewaypb client type —
// never services/card-issuance's or services/gateway's internal
// business-logic packages directly — enforcing the boundary ADR 0003's
// Consequences section calls for.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// GatewayClient is the generated gatewaypb.GatewayClient restated here
// (identical method set) so a fake implementing just this interface is
// enough for this package's tests — no bufconn/real gRPC server
// required, mirroring the narrow-interface style of
// internal/grpcserver's own CardIssuanceClient/LedgerClient.
type GatewayClient interface {
	IssueCard(ctx context.Context, in *gatewaypb.IssueCardRequest, opts ...grpc.CallOption) (*gatewaypb.Card, error)
	AdjustCardLimit(ctx context.Context, in *gatewaypb.AdjustCardLimitRequest, opts ...grpc.CallOption) (*gatewaypb.Card, error)
	GetCard(ctx context.Context, in *gatewaypb.GetCardRequest, opts ...grpc.CallOption) (*gatewaypb.Card, error)
	ListCards(ctx context.Context, in *gatewaypb.ListCardsRequest, opts ...grpc.CallOption) (*gatewaypb.ListCardsResponse, error)
	RationBridgeFunding(ctx context.Context, in *gatewaypb.RationBridgeFundingRequest, opts ...grpc.CallOption) (*gatewaypb.RationBridgeFundingResponse, error)
	SimulateDeposit(ctx context.Context, in *gatewaypb.SimulateDepositRequest, opts ...grpc.CallOption) (*gatewaypb.SimulateDepositResponse, error)
	StreamActivity(ctx context.Context, in *gatewaypb.StreamActivityRequest, opts ...grpc.CallOption) (gatewaypb.Gateway_StreamActivityClient, error)
	GetDashboardSnapshot(ctx context.Context, in *gatewaypb.GetDashboardSnapshotRequest, opts ...grpc.CallOption) (*gatewaypb.GetDashboardSnapshotResponse, error)
}

// Handler serves the REST/JSON facade described in this package's doc
// comment.
type Handler struct {
	client GatewayClient
	log    *slog.Logger

	// dashboardOperatorToken is gateway's OWN operator-scoped credential
	// (see cmd/gateway/main.go's operatorToken, the same one already
	// attached to gateway's own outgoing ledger-core calls) — set via
	// SetDashboardOperatorToken, used ONLY by the dashboard's activity
	// SSE handler (dashboard.go), NEVER by handleStreamActivity's
	// existing, documented /v1/activity route. See dashboard.go's doc
	// comment for exactly why that split is intentional: the dashboard
	// is a deliberately public, read-only, zero-login-friction surface
	// (so it carries gateway's own identity, not a caller-supplied
	// one), while /v1/activity remains a real REST API route that must
	// keep requiring its caller's own forwarded credential — injecting
	// this token there instead would silently turn every REST caller
	// into gateway's operator for that route, undermining the exact
	// per-caller authorization internal/tenantauth's StreamActivity
	// enforcement exists to provide.
	dashboardOperatorToken string
}

// NewHandler constructs a Handler that translates every request into a
// call against client.
func NewHandler(client GatewayClient, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{client: client, log: log}
}

// SetDashboardOperatorToken configures the operator-scoped credential
// the dashboard's live activity stream (dashboard.go's
// handleDashboardActivityStream) presents to gateway's own gRPC
// StreamActivity RPC — see the Handler.dashboardOperatorToken field's
// doc comment for why this exists as its own narrow setter rather than
// a NewHandler constructor parameter (every existing call site, this
// package's own tests included, keeps working unchanged when the
// dashboard isn't exercised). Optional: an empty token leaves the
// dashboard's activity feed non-functional (every SSE connection will
// fail with the same Unauthenticated status any other uncredentialed
// StreamActivity caller gets) without affecting any other route.
func (h *Handler) SetDashboardOperatorToken(token string) {
	h.dashboardOperatorToken = token
}

// forwardedCredentialHeaders are the two HTTP headers a REST caller may
// present a credential under, mirroring internal/tenantauth's
// apiKeyHeader/authorizationHeader gRPC metadata keys exactly (same
// names on the wire, just HTTP headers instead of gRPC metadata) — so a
// credential minted once (an API key or a bearer JWT, see
// proto/auth.proto's IssueAPIKey/MintJWT) works identically whether the
// caller is a gRPC client or an sdk/go/sdk/rust REST client hitting this
// facade.
const (
	forwardedAPIKeyHeader        = "X-Api-Key"
	forwardedAuthorizationHeader = "Authorization"
)

// clientContext builds the context every handler in this package uses
// to call h.client: r's context (so request cancellation/timeout still
// propagates), with whichever of the two credential headers r carries
// forwarded onto the outgoing gRPC call's metadata.
//
// Without this, this REST facade's only RPCs would be the ones gateway's
// own internal/tenantauth interceptor doesn't scope to a tenant — every
// tenant-scoped RPC behind cfg.RequireTenantAuth would 401 for every
// REST caller regardless of what credential it presented, because
// cmd/gateway/main.go's loopback connection from this process back to
// its own gRPC listener (selfConn) carries no credential of its own by
// design: this facade is a translation layer, not a privileged bypass
// of the same tenant-scoping every gRPC caller is held to.
func clientContext(r *http.Request) context.Context {
	ctx := r.Context()
	if apiKey := r.Header.Get(forwardedAPIKeyHeader); apiKey != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-api-key", apiKey)
	}
	if authz := r.Header.Get(forwardedAuthorizationHeader); authz != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", authz)
	}
	return ctx
}

// Mux returns an http.Handler with every route in this package's doc
// comment registered, ready to be wrapped in an http.Server by
// cmd/gateway/main.go — the same pattern as
// services/webhook-ingestion/internal/webhookhandler's Mux.
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/cards", h.handleIssueCard)
	mux.HandleFunc("GET /v1/cards/{card_id}", h.handleGetCard)
	mux.HandleFunc("PATCH /v1/cards/{card_id}/limit", h.handleAdjustCardLimit)
	mux.HandleFunc("GET /v1/tenants/{tenant_id}/cards", h.handleListCards)
	mux.HandleFunc("POST /v1/tenants/{tenant_id}/bridge-funding", h.handleRationBridgeFunding)
	mux.HandleFunc("POST /v1/tenants/{tenant_id}/deposits/simulate", h.handleSimulateDeposit)
	mux.HandleFunc("GET /v1/activity", h.handleStreamActivity)
	mux.HandleFunc("GET /v1/dashboard", h.handleDashboardPage)
	mux.HandleFunc("GET /v1/dashboard/snapshot", h.handleDashboardSnapshot)
	mux.HandleFunc("GET /v1/dashboard/activity/stream", h.handleDashboardActivityStream)
	return mux
}
