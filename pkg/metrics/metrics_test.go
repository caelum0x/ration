package metrics

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
)

func TestBridgeGrantsTotalIncrementsAndExports(t *testing.T) {
	m := New("test-service")

	m.BridgeGrantsTotal.WithLabelValues("granted").Inc()
	m.BridgeGrantsTotal.WithLabelValues("granted").Inc()
	m.BridgeGrantsTotal.WithLabelValues("declined").Inc()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `ration_bridge_grants_total{result="granted",service="test-service"} 2`) {
		t.Fatalf("expected granted=2 in /metrics output, got:\n%s", body)
	}
	if !strings.Contains(body, `ration_bridge_grants_total{result="declined",service="test-service"} 1`) {
		t.Fatalf("expected declined=1 in /metrics output, got:\n%s", body)
	}
}

func TestReserveFloorHeadroomGaugeSetsPerTenant(t *testing.T) {
	m := New("test-service")

	m.ReserveFloorHeadroom.WithLabelValues("tenant-a").Set(12345)
	m.ReserveFloorHeadroom.WithLabelValues("tenant-b").Set(0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `ration_reserve_floor_headroom_minor_units{service="test-service",tenant_id="tenant-a"} 12345`) {
		t.Fatalf("expected tenant-a headroom in /metrics output, got:\n%s", body)
	}
}

func TestUnaryServerInterceptorRecordsSuccessAndFailure(t *testing.T) {
	m := New("test-service")
	interceptor := m.UnaryServerInterceptor()

	okHandler := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	if _, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/svc/Ok"}, okHandler); err != nil {
		t.Fatalf("unexpected error from ok handler: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	m.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `method="/svc/Ok"`) {
		t.Fatalf("expected a request_total series for /svc/Ok, got:\n%s", body)
	}
}

func TestServerMountsMetricsEndpoint(t *testing.T) {
	m := New("test-service")
	srv := m.Server("127.0.0.1:0")
	if srv.Handler == nil {
		t.Fatal("Server() returned an http.Server with no handler")
	}
}
