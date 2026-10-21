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
)

// fakeVerifier is a scriptable Verifier — no real auth service
// involved, this package's own logic is what's under test.
type fakeVerifier struct {
	apiKeyTenant string
	apiKeyValid  bool
	apiKeyErr    error

	jwtTenant string
	jwtValid  bool
	jwtErr    error
}

func (f *fakeVerifier) VerifyAPIKey(ctx context.Context, secret string) (string, bool, string, error) {
	if f.apiKeyErr != nil {
		return "", false, "", f.apiKeyErr
	}
	if !f.apiKeyValid {
		return "", false, "revoked", nil
	}
	return f.apiKeyTenant, true, "", nil
}

func (f *fakeVerifier) VerifyJWT(ctx context.Context, token string) (string, bool, string, error) {
	if f.jwtErr != nil {
		return "", false, "", f.jwtErr
	}
	if !f.jwtValid {
		return "", false, "expired", nil
	}
	return f.jwtTenant, true, "", nil
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
	interceptor := UnaryServerInterceptor(&fakeVerifier{}, false)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1", RequestedMinorUnits: 100}

	_, err := interceptor(context.Background(), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil when required=false", err)
	}
}

func TestUnaryServerInterceptor_Required_MissingCredentialRejected(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{}, true)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(context.Background(), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("interceptor() error = %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptor_Required_ValidAPIKeyMatchingTenantAllowed(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true}, true)
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
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyTenant: "tenant_1", apiKeyValid: true}, true)
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
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyValid: false}, true)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(ctxWithAPIKey("wrong-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("interceptor() error = %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptor_Required_VerifierErrorRejected(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyErr: errors.New("auth unreachable")}, true)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("interceptor() error = %v, want Unauthenticated", err)
	}
}

func TestUnaryServerInterceptor_Required_ValidBearerJWTMatchingTenantAllowed(t *testing.T) {
	interceptor := UnaryServerInterceptor(&fakeVerifier{jwtTenant: "tenant_3", jwtValid: true}, true)
	req := &gatewaypb.SimulateDepositRequest{TenantId: "tenant_3", AmountMinorUnits: 500}

	_, err := interceptor(ctxWithBearer("good-token"), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil", err)
	}
}

func TestUnaryServerInterceptor_Required_UnscopedRequestPassesThrough(t *testing.T) {
	// GetCardRequest carries only card_id, not tenant_id (see
	// requestTenantID's doc comment) — this is the documented,
	// deliberate gap, not a bug: it should pass through even with no
	// credential presented at all.
	interceptor := UnaryServerInterceptor(&fakeVerifier{}, true)
	req := &gatewaypb.GetCardRequest{CardId: "card_1"}

	_, err := interceptor(context.Background(), req, unaryInfo(), handlerEcho)
	if err != nil {
		t.Fatalf("interceptor() error = %v, want nil for an unscoped request", err)
	}
}

func TestUnaryServerInterceptor_Required_EmptyVerifiedTenantRejected(t *testing.T) {
	// A credential that verifies but reports no tenant_id of its own
	// must never be treated as "authorized for every tenant."
	interceptor := UnaryServerInterceptor(&fakeVerifier{apiKeyTenant: "", apiKeyValid: true}, true)
	req := &gatewaypb.RationBridgeFundingRequest{TenantId: "tenant_1"}

	_, err := interceptor(ctxWithAPIKey("good-secret"), req, unaryInfo(), handlerEcho)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("interceptor() error = %v, want PermissionDenied", err)
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
