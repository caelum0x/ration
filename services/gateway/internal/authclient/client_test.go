package authclient

import (
	"context"
	"net"
	"testing"

	authpb "github.com/caelum0x/ration/pkg/pb/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeAuthServer is a minimal real gRPC server implementing enough of
// AuthServer to exercise Client's RPC wrapping over an actual gRPC
// connection (via bufconn, no real network socket), rather than
// mocking Client itself.
type fakeAuthServer struct {
	authpb.UnimplementedAuthServer
	apiKeyTenant string // VerifyAPIKey returns valid=true for this tenant iff secret == "good-secret"
	jwtTenant    string // VerifyJWT returns valid=true for this tenant iff token == "good-token"
}

func (f *fakeAuthServer) VerifyAPIKey(ctx context.Context, req *authpb.VerifyAPIKeyRequest) (*authpb.VerifyAPIKeyResponse, error) {
	if req.GetSecret() != "good-secret" {
		return &authpb.VerifyAPIKeyResponse{Valid: false, Reason: "not found"}, nil
	}
	return &authpb.VerifyAPIKeyResponse{Valid: true, TenantId: f.apiKeyTenant, KeyId: "key_1"}, nil
}

func (f *fakeAuthServer) VerifyJWT(ctx context.Context, req *authpb.VerifyJWTRequest) (*authpb.VerifyJWTResponse, error) {
	if req.GetToken() != "good-token" {
		return &authpb.VerifyJWTResponse{Valid: false, Reason: "bad signature"}, nil
	}
	return &authpb.VerifyJWTResponse{Valid: true, TenantId: f.jwtTenant, Subject: "operator_1"}, nil
}

func newTestClient(t *testing.T, server *fakeAuthServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	authpb.RegisterAuthServer(grpcServer, server)
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

func TestClient_VerifyAPIKey_Valid(t *testing.T) {
	client := newTestClient(t, &fakeAuthServer{apiKeyTenant: "tenant_1"})

	tenantID, valid, _, _, err := client.VerifyAPIKey(context.Background(), "good-secret")
	if err != nil {
		t.Fatalf("VerifyAPIKey() error = %v", err)
	}
	if !valid || tenantID != "tenant_1" {
		t.Errorf("VerifyAPIKey() = (tenantID=%q, valid=%v), want (tenant_1, true)", tenantID, valid)
	}
}

func TestClient_VerifyAPIKey_Invalid(t *testing.T) {
	client := newTestClient(t, &fakeAuthServer{apiKeyTenant: "tenant_1"})

	_, valid, _, reason, err := client.VerifyAPIKey(context.Background(), "wrong-secret")
	if err != nil {
		t.Fatalf("VerifyAPIKey() error = %v", err)
	}
	if valid {
		t.Errorf("VerifyAPIKey() valid = true, want false (reason=%q)", reason)
	}
}

func TestClient_VerifyJWT_Valid(t *testing.T) {
	client := newTestClient(t, &fakeAuthServer{jwtTenant: "tenant_2"})

	tenantID, valid, _, _, err := client.VerifyJWT(context.Background(), "good-token")
	if err != nil {
		t.Fatalf("VerifyJWT() error = %v", err)
	}
	if !valid || tenantID != "tenant_2" {
		t.Errorf("VerifyJWT() = (tenantID=%q, valid=%v), want (tenant_2, true)", tenantID, valid)
	}
}

func TestClient_VerifyJWT_Invalid(t *testing.T) {
	client := newTestClient(t, &fakeAuthServer{jwtTenant: "tenant_2"})

	_, valid, _, reason, err := client.VerifyJWT(context.Background(), "wrong-token")
	if err != nil {
		t.Fatalf("VerifyJWT() error = %v", err)
	}
	if valid {
		t.Errorf("VerifyJWT() valid = true, want false (reason=%q)", reason)
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
