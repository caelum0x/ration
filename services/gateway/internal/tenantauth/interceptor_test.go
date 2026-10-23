package tenantauth

import (
	"context"
	"errors"
	"testing"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakeVerifier is a scriptable Verifier — no real auth service
// involved, this package's own logic is what's under test.
type fakeVerifier struct {
	apiKeyTenant     string
	apiKeyValid      bool
	apiKeyIsOperator bool
	apiKeyErr        error

	jwtTenant     string
	jwtValid      bool
	jwtIsOperator bool
	jwtErr        error
}

func (f *fakeVerifier) VerifyAPIKey(ctx context.Context, secret string) (string, bool, bool, string, error) {
	if f.apiKeyErr != nil {
		return "", false, false, "", f.apiKeyErr
	}
	if !f.apiKeyValid {
		return "", false, false, "revoked", nil
	}
	return f.apiKeyTenant, true, f.apiKeyIsOperator, "", nil
}

func (f *fakeVerifier) VerifyJWT(ctx context.Context, token string) (string, bool, bool, string, error) {
	if f.jwtErr != nil {
		return "", false, false, "", f.jwtErr
	}
	if !f.jwtValid {
		return "", false, false, "expired", nil
	}
	return f.jwtTenant, true, f.jwtIsOperator, "", nil
}

func handlerEcho(ctx context.Context, req any) (any, error) {
	return req, nil
}

func unaryInfo() *grpc.UnaryServerInfo {
	return &grpc.UnaryServerInfo{FullMethod: "/ration.gateway.v1.Gateway/RationBridgeFunding"}
}

func ctxWithAPIKey(key string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(apiKeyHeader, key))
}

func ctxWithBearer(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(authorizationHeader, bearerPrefix+token))
}

func TestUnaryServerInterceptor_Disabled_PassesThroughWithoutCredential(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{}, false, nil)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1", RequestedMinorUnits: 100}

	_, err := interceptor(context.Background(), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil when required=false", err)
	}
}

func TestUnaryServerInterceptor_Required_MissingCredentialRejected(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{}, true, nil)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(context.Background(), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("interceptor() error = %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptor_Required_ValidAPIKeyMatchingTenantAllowed(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true}, true, nil)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1", RequestedMinorUnits: 100}

	resp, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil", err)
	}
	if resp.(*gatewaypb.RationBridgeFundingRequest).GetTenantId() != "tenant_1" {
		t.Fatalf("handler was not invoked with the original request")
	}
}

func TestUnaryServerInterceptor_Required_APIKeyWrongTenantRejected(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true}, true, nil)
	// This is the core of docs/THREAT-MODEL.md item 1: a credential
	// that verifies, but for a *different* tenant than the one named in
	// the request, must be rejected — not merely authenticated.
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_2", RequestedMinorUnits: 100}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor() error = %v, want PermissionDenied", err)
	}
}

func TestUnaryServerInterceptor_Required_InvalidAPIKeyRejected(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyValid: false}, true, nil)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(ctxWithAPIKey("wrong-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("interceptor() error = %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptor_Required_VerifierErrorRejected(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyErr: errors.New("auth unreachable")}, true, nil)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("interceptor() error = %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptor_Required_ValidBearerJWTMatchingTenantAllowed(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{jwtTenant: "tenant_3", jwtValid: true}, true, nil)
	req := &gatewaypb.SimulateDepositRequest{TenantId: "tenant_3", AmountMinorUnits: 500}

	_, err := interceptor(ctxWithBearer("good-token"), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil", err)
	}
}

func TestUnaryServerInterceptor_Required_UnscopedRequestPassesThrough(t *testing.T) {
	// GetCardRequest carries only card_id, not tenant_id (see
	// requestTenantID's doc comment); with no cardResolver configured
	// (nil, as production never passes today) there is no way to
	// resolve one, so it passes through even with no credential
	// presented at all. See the CardResolver-suffixed tests below for
	// the case where one IS configured.
	interceptor := UnaryServerInterceptor(&fakeVerifier{}, true, nil)
	req := &gatewaypb.GetCardRequest{CardId: "card_1"}

	_, err := interceptor(context.Background(), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil for an unscoped request", err)
	}
}

func TestUnaryServerInterceptor_Required_OperatorCredentialBypassesTenantMatch(t *testing.T) {
	// An operator-scoped credential (is_operator=true) authenticates
	// with no tenant_id of its own and must be allowed to act on behalf
	// of ANY tenant_id a request carries — this is what lets the CLI/TUI
	// operator dashboards and the agent's polling loop keep working once
	// per-tenant matching is enforced for everyone else.
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyValid: true, apiKeyIsOperator: true}, true, nil)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1", RequestedMinorUnits: 100}

	resp, err := interceptor(ctxWithAPIKey("good-operator-secret"), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil for an operator credential", err)
	}
	if resp.(*gatewaypb.RationBridgeFundingRequest).GetTenantId() != "tenant_1" {
		t.Fatalf("handler was not invoked with the original request")
	}
}

func TestUnaryServerInterceptor_Required_OperatorBearerJWTBypassesTenantMatch(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{jwtValid: true, jwtIsOperator: true}, true, nil)
	req := &gatewaypb.SimulateDepositRequest{TenantId: "tenant_9", AmountMinorUnits: 500}

	_, err := interceptor(ctxWithBearer("good-operator-token"), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil for an operator JWT", err)
	}
}

func TestUnaryServerInterceptor_Required_EmptyVerifiedTenantRejected(t *testing.T) {
	// A credential that verifies but reports no tenant_id of its own
	// must never be treated as "authorized for every tenant."
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyTenant: "", apiKeyValid: true}, true, nil)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor() error = %v, want PermissionDenied", err)
	}
}

// fakeCardResolver is a scriptable cardtenant.Resolver — no real
// card-issuance involved, this package's own card_id-resolution wiring
// is what's under test.
type fakeCardResolver struct {
	tenantByCard map[string]string
	err          error
}

func (f *fakeCardResolver) ResolveTenantID(ctx context.Context, cardID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.tenantByCard[cardID], nil
}

func TestUnaryServerInterceptor_Required_CardResolverMatchingTenantAllowed(t *testing.T) {
	interceptor := UnaryServerInterceptor(
		&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true},
		true,
		&fakeCardResolver{tenantByCard: map[string]string{"card_1": "tenant_1"}},
	)
	req := &gatewaypb.GetCardRequest{CardId: "card_1"}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil when the resolved tenant matches the credential", err)
	}
}

func TestUnaryServerInterceptor_Required_CardResolverMismatchedTenantRejected(t *testing.T) {
	// The exact blind spot docs/THREAT-MODEL.md item 1(a) names: a
	// credential valid for tenant_1 must not be able to act on a card
	// that actually belongs to tenant_2, now that card_id resolves.
	interceptor := UnaryServerInterceptor(
		&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true},
		true,
		&fakeCardResolver{tenantByCard: map[string]string{"card_1": "tenant_2"}},
	)
	req := &gatewaypb.AdjustCardLimitRequest{CardId: "card_1", NewLimitMinorUnits: 500}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor() error = %v, want PermissionDenied", err)
	}
}

func TestUnaryServerInterceptor_Required_CardResolverOperatorCredentialAllowed(t *testing.T) {
	interceptor := UnaryServerInterceptor(
		&fakeVerifier{apiKeyValid: true, apiKeyIsOperator: true},
		true,
		&fakeCardResolver{tenantByCard: map[string]string{"card_1": "tenant_2"}},
	)
	req := &gatewaypb.AdjustCardLimitRequest{CardId: "card_1", NewLimitMinorUnits: 500}

	_, err := interceptor(ctxWithAPIKey("good-operator-secret"), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil for an operator credential regardless of card owner", err)
	}
}

func TestUnaryServerInterceptor_Required_CardResolverErrorRejectedNotPassedThrough(t *testing.T) {
	// An unresolvable card_id must never silently become "unscoped, so
	// let it through" — see internal/cardtenant's doc comment.
	interceptor := UnaryServerInterceptor(
		&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true},
		true,
		&fakeCardResolver{err: errors.New("card-issuance unreachable")},
	)
	req := &gatewaypb.GetCardRequest{CardId: "card_1"}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor() error = %v, want PermissionDenied when the card can't be resolved", err)
	}
}

func TestRequestCardID(t *testing.T) {
	if id, ok := requestCardID(&gatewaypb.GetCardRequest{CardId: "card_1"}); !ok || id != "card_1" {
		t.Errorf("requestCardID() = (%q, %v), want (card_1, true)", id, ok)
	}
	if _, ok := requestCardID(&gatewaypb.RationBridgeFundingRequest{TenantId: "t1"}); ok {
		t.Errorf("requestCardID() ok = true for a request with no card_id, want false")
	}
}

func TestRequestTenantID(t *testing.T) {
	if id, ok := requestTenantID(&gatewaypb.RationBridgeFundingRequest{TenantId: "t1"}); !ok || id != "t1" {
		t.Errorf("requestTenantID() = (%q, %v), want (t1, true)", id, ok)
	}
	if _, ok := requestTenantID(&gatewaypb.GetCardRequest{CardId: "c1"}); ok {
		t.Errorf("requestTenantID() ok = true for GetCardRequest, want false")
	}
	if _, ok := requestTenantID("not-a-proto-message"); ok {
		t.Errorf("requestTenantID() ok = true for a non-proto value, want false")
	}
}

// fakeServerStream is a minimal grpc.ServerStream whose RecvMsg decodes
// a single scripted request (mimicking the generated StreamActivity
// handler's "recv exactly one request, then stream forever" shape) into
// whatever message pointer the caller passes, by proto.Merge.
type fakeServerStream struct {
	grpc.ServerStream
	ctx     context.Context
	request proto.Message
	recvd   bool
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func (f *fakeServerStream) RecvMsg(m any) error {
	if f.recvd {
		return context.Canceled // only one request for this RPC shape
	}
	f.recvd = true
	if dst, ok := m.(proto.Message); ok {
		proto.Merge(dst, f.request)
	}
	return nil
}

func streamInfo() *grpc.StreamServerInfo {
	return &grpc.StreamServerInfo{FullMethod: "/ration.gateway.v1.Gateway/StreamActivity", IsServerStream: true}
}

func streamHandlerRecvOnce(srv any, stream grpc.ServerStream) error {
	var req gatewaypb.StreamActivityRequest
	return stream.RecvMsg(&req)
}

func TestStreamServerInterceptor_Disabled_PassesThroughWithoutCredential(t *testing.T) {
	interceptor := StreamServerInterceptor(&fakeVerifier{}, false)
	ss := &fakeServerStream{ctx: context.Background(), request: &gatewaypb.StreamActivityRequest{}}

	if err := interceptor(nil, ss, streamInfo(), streamHandlerRecvOnce); err != nil {
		t.Fatalf("interceptor() error = %v, want nil when required=false", err)
	}
}

func TestStreamServerInterceptor_Required_MissingCredentialRejected(t *testing.T) {
	interceptor := StreamServerInterceptor(&fakeVerifier{}, true)
	ss := &fakeServerStream{ctx: context.Background(), request: &gatewaypb.StreamActivityRequest{}}

	err := interceptor(nil, ss, streamInfo(), streamHandlerRecvOnce)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("interceptor() error = %v, want Unauthenticated", err)
	}
}

func TestStreamServerInterceptor_Required_NonOperatorEmptyTenantRejected(t *testing.T) {
	// The exact gap docs/THREAT-MODEL.md item 1(b) names: an all-tenants
	// subscription (empty tenant_id) must not be reachable by a
	// non-operator credential.
	interceptor := StreamServerInterceptor(&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true}, true)
	ss := &fakeServerStream{ctx: ctxWithAPIKey("good-secret"), request: &gatewaypb.StreamActivityRequest{TenantId: ""}}

	err := interceptor(nil, ss, streamInfo(), streamHandlerRecvOnce)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor() error = %v, want PermissionDenied for an empty tenant_id", err)
	}
}

func TestStreamServerInterceptor_Required_NonOperatorMismatchedTenantRejected(t *testing.T) {
	interceptor := StreamServerInterceptor(&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true}, true)
	ss := &fakeServerStream{ctx: ctxWithAPIKey("good-secret"), request: &gatewaypb.StreamActivityRequest{TenantId: "tenant_2"}}

	err := interceptor(nil, ss, streamInfo(), streamHandlerRecvOnce)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor() error = %v, want PermissionDenied for a mismatched tenant_id", err)
	}
}

func TestStreamServerInterceptor_Required_NonOperatorOwnTenantAllowed(t *testing.T) {
	interceptor := StreamServerInterceptor(&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true}, true)
	ss := &fakeServerStream{ctx: ctxWithAPIKey("good-secret"), request: &gatewaypb.StreamActivityRequest{TenantId: "tenant_1"}}

	if err := interceptor(nil, ss, streamInfo(), streamHandlerRecvOnce); err != nil {
		t.Fatalf("interceptor() error = %v, want nil for a credential subscribing to its own tenant", err)
	}
}

func TestStreamServerInterceptor_Required_OperatorEmptyTenantAllowed(t *testing.T) {
	// The operator-dashboard case: an operator credential subscribing to
	// every tenant's activity (empty tenant_id) must keep working.
	interceptor := StreamServerInterceptor(&fakeVerifier{apiKeyValid: true, apiKeyIsOperator: true}, true)
	ss := &fakeServerStream{ctx: ctxWithAPIKey("good-operator-secret"), request: &gatewaypb.StreamActivityRequest{TenantId: ""}}

	if err := interceptor(nil, ss, streamInfo(), streamHandlerRecvOnce); err != nil {
		t.Fatalf("interceptor() error = %v, want nil for an operator credential with an empty tenant_id", err)
	}
}
