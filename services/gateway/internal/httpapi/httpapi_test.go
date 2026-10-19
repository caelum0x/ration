package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	commonpb "github.com/caelum0x/ration/pkg/pb/common"
	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeGatewayClient implements GatewayClient directly (no gRPC
// transport at all) — httpapi's own tests only need to prove the
// JSON<->gRPC-request translation, not gRPC wire behavior (that is
// exactly what sdk/go/ration's and internal/cardissuanceclient's own
// bufconn-based tests already prove for their respective layers).
type fakeGatewayClient struct {
	cards            map[string]*gatewaypb.Card
	nextCardID       int
	events           []*gatewaypb.ActivityEvent
	rationBridgeErr  error
	getCardErr       error
	issueCardErr     error
	simulateDeposits int
}

func (f *fakeGatewayClient) IssueCard(ctx context.Context, in *gatewaypb.IssueCardRequest, opts ...grpc.CallOption) (*gatewaypb.Card, error) {
	if f.issueCardErr != nil {
		return nil, f.issueCardErr
	}
	if f.cards == nil {
		f.cards = map[string]*gatewaypb.Card{}
	}
	f.nextCardID++
	c := &gatewaypb.Card{
		Id:              fmt.Sprintf("card_%d", f.nextCardID),
		TenantId:        in.GetTenantId(),
		CardholderName:  in.GetCardholderName(),
		Status:          commonpb.CardStatus_CARD_STATUS_ACTIVE,
		LimitMinorUnits: in.GetLimitMinorUnits(),
		LimitInterval:   in.GetLimitInterval(),
		Currency:        in.GetCurrency(),
	}
	f.cards[c.Id] = c
	return c, nil
}

func (f *fakeGatewayClient) AdjustCardLimit(ctx context.Context, in *gatewaypb.AdjustCardLimitRequest, opts ...grpc.CallOption) (*gatewaypb.Card, error) {
	c, ok := f.cards[in.GetCardId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "card not found")
	}
	c.LimitMinorUnits = in.GetNewLimitMinorUnits()
	return c, nil
}

func (f *fakeGatewayClient) GetCard(ctx context.Context, in *gatewaypb.GetCardRequest, opts ...grpc.CallOption) (*gatewaypb.Card, error) {
	if f.getCardErr != nil {
		return nil, f.getCardErr
	}
	return f.cards[in.GetCardId()], nil
}

func (f *fakeGatewayClient) ListCards(ctx context.Context, in *gatewaypb.ListCardsRequest, opts ...grpc.CallOption) (*gatewaypb.ListCardsResponse, error) {
	var out []*gatewaypb.Card
	for _, c := range f.cards {
		if c.GetTenantId() == in.GetTenantId() {
			out = append(out, c)
		}
	}
	return &gatewaypb.ListCardsResponse{Cards: out}, nil
}

func (f *fakeGatewayClient) RationBridgeFunding(ctx context.Context, in *gatewaypb.RationBridgeFundingRequest, opts ...grpc.CallOption) (*gatewaypb.RationBridgeFundingResponse, error) {
	if f.rationBridgeErr != nil {
		return nil, f.rationBridgeErr
	}
	return &gatewaypb.RationBridgeFundingResponse{
		GrantedMinorUnits: in.GetRequestedMinorUnits(),
		FullyGranted:      true,
		Explanation:       "granted in full (fake)",
	}, nil
}

func (f *fakeGatewayClient) SimulateDeposit(ctx context.Context, in *gatewaypb.SimulateDepositRequest, opts ...grpc.CallOption) (*gatewaypb.SimulateDepositResponse, error) {
	f.simulateDeposits++
	return &gatewaypb.SimulateDepositResponse{DepositId: fmt.Sprintf("deposit_%d", f.simulateDeposits)}, nil
}

// fakeStream implements gatewaypb.Gateway_StreamActivityClient over an
// in-memory slice — enough to exercise handleStreamActivity's Recv loop
// without a real gRPC connection.
type fakeStream struct {
	grpc.ClientStream
	events []*gatewaypb.ActivityEvent
	idx    int
}

func (s *fakeStream) Recv() (*gatewaypb.ActivityEvent, error) {
	if s.idx >= len(s.events) {
		return nil, io.EOF
	}
	e := s.events[s.idx]
	s.idx++
	return e, nil
}

func (f *fakeGatewayClient) StreamActivity(ctx context.Context, in *gatewaypb.StreamActivityRequest, opts ...grpc.CallOption) (gatewaypb.Gateway_StreamActivityClient, error) {
	var matched []*gatewaypb.ActivityEvent
	for _, e := range f.events {
		if in.GetTenantId() != "" && e.GetTenantId() != in.GetTenantId() {
			continue
		}
		matched = append(matched, e)
	}
	return &fakeStream{events: matched}, nil
}

func TestHandleIssueCard(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)

	body := `{"tenant_id":"tenant_1","cardholder_name":"Ada Lovelace","limit_minor_units":50000,"limit_interval":"monthly","currency":"USD"}`
	req := httptest.NewRequest("POST", "/v1/cards", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got cardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.CardholderName != "Ada Lovelace" || got.TenantID != "tenant_1" {
		t.Errorf("got = %+v, want Ada Lovelace/tenant_1", got)
	}
	if got.Status != "CARD_STATUS_ACTIVE" {
		t.Errorf("Status = %q, want CARD_STATUS_ACTIVE", got.Status)
	}
}

func TestHandleIssueCard_MissingRequiredField(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)

	body := `{"cardholder_name":"Ada Lovelace","limit_minor_units":50000,"currency":"USD"}`
	req := httptest.NewRequest("POST", "/v1/cards", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleIssueCard_MalformedJSON(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)

	req := httptest.NewRequest("POST", "/v1/cards", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()

	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetCard(t *testing.T) {
	client := &fakeGatewayClient{}
	h := NewHandler(client, nil)

	card, err := client.IssueCard(context.Background(), &gatewaypb.IssueCardRequest{TenantId: "tenant_1", CardholderName: "Ada", LimitMinorUnits: 1000, Currency: "USD"})
	if err != nil {
		t.Fatalf("seed IssueCard: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/cards/"+card.GetId(), nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got cardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != card.GetId() {
		t.Errorf("ID = %q, want %q", got.ID, card.GetId())
	}
}

func TestHandleGetCard_NotFound(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)

	req := httptest.NewRequest("GET", "/v1/cards/does-not-exist", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetCard_GRPCError(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{getCardErr: status.Error(codes.PermissionDenied, "nope")}, nil)

	req := httptest.NewRequest("GET", "/v1/cards/card_1", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Code != "PermissionDenied" {
		t.Errorf("Error.Code = %q, want PermissionDenied", body.Error.Code)
	}
}

func TestHandleAdjustCardLimit(t *testing.T) {
	client := &fakeGatewayClient{}
	h := NewHandler(client, nil)

	card, err := client.IssueCard(context.Background(), &gatewaypb.IssueCardRequest{TenantId: "tenant_1", CardholderName: "Ada", LimitMinorUnits: 1000, Currency: "USD"})
	if err != nil {
		t.Fatalf("seed IssueCard: %v", err)
	}

	req := httptest.NewRequest("PATCH", "/v1/cards/"+card.GetId()+"/limit", strings.NewReader(`{"new_limit_minor_units":5000,"reason":"test"}`))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got cardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.LimitMinorUnits != 5000 {
		t.Errorf("LimitMinorUnits = %d, want 5000", got.LimitMinorUnits)
	}
}

func TestHandleListCards(t *testing.T) {
	client := &fakeGatewayClient{}
	h := NewHandler(client, nil)

	for i := 0; i < 2; i++ {
		if _, err := client.IssueCard(context.Background(), &gatewaypb.IssueCardRequest{TenantId: "tenant_1", CardholderName: "Ada", LimitMinorUnits: 1000, Currency: "USD"}); err != nil {
			t.Fatalf("seed IssueCard: %v", err)
		}
	}

	req := httptest.NewRequest("GET", "/v1/tenants/tenant_1/cards", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got listCardsResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Cards) != 2 {
		t.Errorf("len(Cards) = %d, want 2", len(got.Cards))
	}
}

func TestHandleRationBridgeFunding(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)

	req := httptest.NewRequest("POST", "/v1/tenants/tenant_1/bridge-funding", strings.NewReader(`{"requested_minor_units":5000,"reason":"payout delayed"}`))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got rationBridgeFundingResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.GrantedMinorUnits != 5000 || !got.FullyGranted {
		t.Errorf("got = %+v, want granted=5000 fully_granted=true", got)
	}
}

func TestHandleRationBridgeFunding_ResourceExhausted(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{rationBridgeErr: status.Error(codes.ResourceExhausted, "rate limited")}, nil)

	req := httptest.NewRequest("POST", "/v1/tenants/tenant_1/bridge-funding", strings.NewReader(`{"requested_minor_units":5000,"reason":"x"}`))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSimulateDeposit(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)

	req := httptest.NewRequest("POST", "/v1/tenants/tenant_1/deposits/simulate", strings.NewReader(`{"amount_minor_units":50000,"mark_late":true}`))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got simulateDepositResponseJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.DepositID == "" {
		t.Error("DepositID is empty")
	}
}

func TestHandleSimulateDeposit_InvalidAmount(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)

	req := httptest.NewRequest("POST", "/v1/tenants/tenant_1/deposits/simulate", strings.NewReader(`{"amount_minor_units":0}`))
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleStreamActivity(t *testing.T) {
	client := &fakeGatewayClient{
		events: []*gatewaypb.ActivityEvent{
			{Id: "evt_1", TenantId: "tenant_1", Kind: "card", Summary: "issued"},
			{Id: "evt_2", TenantId: "tenant_2", Kind: "card", Summary: "issued"},
		},
	}
	h := NewHandler(client, nil)

	req := httptest.NewRequest("GET", "/v1/activity?tenant_id=tenant_1", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}

	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	var got []activityEventJSON
	for {
		var e activityEventJSON
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode ndjson line: %v", err)
		}
		got = append(got, e)
	}
	if len(got) != 1 || got[0].ID != "evt_1" {
		t.Errorf("got = %+v, want exactly evt_1", got)
	}
}

func TestHandleListCards_MissingTenantID(t *testing.T) {
	h := NewHandler(&fakeGatewayClient{}, nil)
	req := httptest.NewRequest("GET", "/v1/tenants//cards", nil)
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	// Go's ServeMux treats an empty {tenant_id} segment as a 404 (no
	// route match) rather than reaching the handler at all — assert
	// the facade never serves a 200 with an empty tenant_id, which is
	// the property that actually matters here.
	if rec.Code == 200 {
		t.Fatalf("status = %d, want not-200 for an empty tenant_id segment", rec.Code)
	}
}
