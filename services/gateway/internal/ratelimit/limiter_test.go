package ratelimit

import (
	"context"
	"testing"
	"time"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeClock lets tests advance time deterministically instead of
// sleeping real wall-clock time.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(rate, burst float64) (*Limiter, *fakeClock) {
	l := New(rate, burst)
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l.now = clock.now
	return l, clock
}

func TestAllow_BurstThenExhausted(t *testing.T) {
	l, _ := newTestLimiter(1, 3)
	for i := 0; i < 3; i++ {
		if !l.Allow("tenant:a") {
			t.Fatalf("expected request %d within burst to be allowed", i)
		}
	}
	if l.Allow("tenant:a") {
		t.Fatalf("expected request beyond burst to be denied")
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	l, clock := newTestLimiter(2 /* tokens/sec */, 2)
	if !l.Allow("tenant:a") || !l.Allow("tenant:a") {
		t.Fatalf("expected initial burst of 2 to be allowed")
	}
	if l.Allow("tenant:a") {
		t.Fatalf("expected bucket to be empty")
	}

	clock.advance(500 * time.Millisecond) // refills 1 token at 2/sec
	if !l.Allow("tenant:a") {
		t.Fatalf("expected 1 token to have refilled after 500ms at 2/sec")
	}
	if l.Allow("tenant:a") {
		t.Fatalf("expected bucket to be empty again immediately after")
	}
}

func TestAllow_RefillCappedAtBurst(t *testing.T) {
	l, clock := newTestLimiter(100, 2)
	l.Allow("tenant:a") // tokens: 1 remaining
	clock.advance(10 * time.Second)
	// Even though 100*10=1000 tokens worth of time passed, the bucket
	// caps at BurstSize.
	if !l.Allow("tenant:a") || !l.Allow("tenant:a") {
		t.Fatalf("expected refill to cap at burst size (2), not grow unbounded")
	}
	if l.Allow("tenant:a") {
		t.Fatalf("expected exactly burst-size tokens available after a long idle period")
	}
}

func TestAllow_KeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	if !l.Allow("tenant:a") {
		t.Fatalf("expected tenant:a's first request to be allowed")
	}
	if !l.Allow("tenant:b") {
		t.Fatalf("expected tenant:b to have its own independent bucket")
	}
}

func TestSweep_EvictsOnlyIdleBuckets(t *testing.T) {
	l, clock := newTestLimiter(1, 1)
	l.Allow("tenant:idle")
	clock.advance(time.Hour)
	l.Allow("tenant:active")

	l.Sweep(30 * time.Minute)

	l.mu.Lock()
	_, idleStillPresent := l.buckets["tenant:idle"]
	_, activeStillPresent := l.buckets["tenant:active"]
	l.mu.Unlock()

	if idleStillPresent {
		t.Fatalf("expected idle bucket to be swept")
	}
	if !activeStillPresent {
		t.Fatalf("expected recently-used bucket to survive the sweep")
	}
}

func TestKeyFor_UsesTenantIDWhenPresent(t *testing.T) {
	req := &gatewaypb.IssueCardRequest{TenantId: "tenant-123"}
	key := keyFor(context.Background(), req)
	if key != "tenant:tenant-123" {
		t.Fatalf("expected tenant-scoped key, got %q", key)
	}
}

func TestKeyFor_FallsBackToPeerWhenNoTenantID(t *testing.T) {
	req := &gatewaypb.GetCardRequest{CardId: "card-123"}
	key := keyFor(context.Background(), req)
	if key != "peer:unknown" {
		t.Fatalf("expected peer fallback key, got %q", key)
	}
}

func TestKeyFor_FallsBackToPeerWhenTenantIDEmpty(t *testing.T) {
	req := &gatewaypb.ListCardsRequest{TenantId: ""}
	key := keyFor(context.Background(), req)
	if key != "peer:unknown" {
		t.Fatalf("expected peer fallback for empty tenant_id, got %q", key)
	}
}

func TestUnaryServerInterceptor_AllowsThenDenies(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	interceptor := UnaryServerInterceptor(l)
	handlerCalls := 0
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalls++
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/ration.gateway.v1.Gateway/IssueCard"}
	req := &gatewaypb.IssueCardRequest{TenantId: "tenant-1"}

	if _, err := interceptor(context.Background(), req, info, handler); err != nil {
		t.Fatalf("expected first call to be allowed, got %v", err)
	}
	_, err := interceptor(context.Background(), req, info, handler)
	if err == nil {
		t.Fatalf("expected second call to be denied")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", status.Code(err))
	}
	if handlerCalls != 1 {
		t.Fatalf("expected handler to be invoked exactly once, got %d", handlerCalls)
	}
}

func TestUnaryServerInterceptor_IndependentTenants(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	interceptor := UnaryServerInterceptor(l)
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/ration.gateway.v1.Gateway/IssueCard"}

	reqA := &gatewaypb.IssueCardRequest{TenantId: "tenant-a"}
	reqB := &gatewaypb.IssueCardRequest{TenantId: "tenant-b"}

	if _, err := interceptor(context.Background(), reqA, info, handler); err != nil {
		t.Fatalf("tenant-a first call: %v", err)
	}
	if _, err := interceptor(context.Background(), reqB, info, handler); err != nil {
		t.Fatalf("expected tenant-b's own bucket to be unaffected by tenant-a's usage: %v", err)
	}
}
