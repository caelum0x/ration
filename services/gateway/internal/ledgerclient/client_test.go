package ledgerclient

import (
	"context"
	"net"
	"testing"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeLedgerCoreServer is a minimal real gRPC server implementing enough
// of LedgerCoreServer to exercise Client's RPC wrapping (request/response
// mapping, pagination loop in ListEntries) over an actual gRPC
// connection (via bufconn, no real network socket), rather than mocking
// Client itself.
type fakeLedgerCoreServer struct {
	ledgerpb.UnimplementedLedgerCoreServer
	balance *ledgerpb.Balance
	entries []*ledgerpb.LedgerEntry
}

func (f *fakeLedgerCoreServer) GetBalance(ctx context.Context, req *ledgerpb.GetBalanceRequest) (*ledgerpb.Balance, error) {
	return f.balance, nil
}

func (f *fakeLedgerCoreServer) ListEntries(ctx context.Context, req *ledgerpb.ListEntriesRequest) (*ledgerpb.ListEntriesResponse, error) {
	// Simulate pagination: one entry per page via numeric page tokens, so
	// Client.ListEntries' page-token loop is actually exercised.
	start := 0
	if req.GetPageToken() != "" {
		start = int(req.GetPageToken()[0] - '0')
	}
	if start >= len(f.entries) {
		return &ledgerpb.ListEntriesResponse{}, nil
	}
	resp := &ledgerpb.ListEntriesResponse{Entries: []*ledgerpb.LedgerEntry{f.entries[start]}}
	if start+1 < len(f.entries) {
		resp.NextPageToken = string(rune('0' + start + 1))
	}
	return resp, nil
}

func (f *fakeLedgerCoreServer) CheckReserveFloor(ctx context.Context, req *ledgerpb.CheckReserveFloorRequest) (*ledgerpb.CheckReserveFloorResponse, error) {
	return &ledgerpb.CheckReserveFloorResponse{Allowed: true, HeadroomMinorUnits: 4242}, nil
}

func newTestClient(t *testing.T, server *fakeLedgerCoreServer) *Client {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	ledgerpb.RegisterLedgerCoreServer(grpcServer, server)
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

func TestClient_GetBalance(t *testing.T) {
	server := &fakeLedgerCoreServer{balance: &ledgerpb.Balance{TenantId: "tenant_1", AvailableMinorUnits: 5000}}
	client := newTestClient(t, server)

	balance, err := client.GetBalance(context.Background(), "tenant_1")
	if err != nil {
		t.Fatalf("GetBalance() error = %v", err)
	}
	if balance.GetAvailableMinorUnits() != 5000 {
		t.Errorf("AvailableMinorUnits = %d, want 5000", balance.GetAvailableMinorUnits())
	}
}

func TestClient_ListEntries_FollowsPagination(t *testing.T) {
	server := &fakeLedgerCoreServer{
		entries: []*ledgerpb.LedgerEntry{
			{Id: "e0"}, {Id: "e1"}, {Id: "e2"},
		},
	}
	client := newTestClient(t, server)

	entries, err := client.ListEntries(context.Background(), "tenant_1")
	if err != nil {
		t.Fatalf("ListEntries() error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	for i, e := range entries {
		want := "e" + string(rune('0'+i))
		if e.GetId() != want {
			t.Errorf("entries[%d].Id = %q, want %q", i, e.GetId(), want)
		}
	}
}

func TestClient_CheckReserveFloor(t *testing.T) {
	server := &fakeLedgerCoreServer{}
	client := newTestClient(t, server)

	resp, err := client.CheckReserveFloor(context.Background(), "tenant_1", 1000)
	if err != nil {
		t.Fatalf("CheckReserveFloor() error = %v", err)
	}
	if !resp.GetAllowed() || resp.GetHeadroomMinorUnits() != 4242 {
		t.Errorf("resp = %+v, want allowed=true headroom=4242", resp)
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
