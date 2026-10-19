package telemetry

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestCorrelationIDRoundTrip(t *testing.T) {
	ctx := context.Background()

	if got := CorrelationIDFromContext(ctx); got != "" {
		t.Fatalf("expected no correlation ID on a bare context, got %q", got)
	}

	id := NewCorrelationID()
	if id == "" {
		t.Fatal("NewCorrelationID returned empty string")
	}

	ctx = ContextWithCorrelationID(ctx, id)
	if got := CorrelationIDFromContext(ctx); got != id {
		t.Fatalf("CorrelationIDFromContext = %q, want %q", got, id)
	}
}

func TestEnsureCorrelationIDGeneratesWhenAbsent(t *testing.T) {
	ctx := ensureCorrelationID(context.Background())

	id := CorrelationIDFromContext(ctx)
	if id == "" {
		t.Fatal("ensureCorrelationID left the context with no correlation ID")
	}
}

func TestEnsureCorrelationIDPreservesIncoming(t *testing.T) {
	const want = "test-correlation-id"
	md := metadata.New(map[string]string{CorrelationIDHeader: want})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	ctx = ensureCorrelationID(ctx)

	if got := CorrelationIDFromContext(ctx); got != want {
		t.Fatalf("ensureCorrelationID() = %q, want %q (should reuse caller's ID)", got, want)
	}
}

func TestInjectCorrelationIDPropagatesToOutgoingMetadata(t *testing.T) {
	id := NewCorrelationID()
	ctx := ContextWithCorrelationID(context.Background(), id)

	ctx = injectCorrelationID(ctx)

	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("expected outgoing metadata to be set")
	}
	got := md.Get(CorrelationIDHeader)
	if len(got) != 1 || got[0] != id {
		t.Fatalf("outgoing metadata %s = %v, want [%s]", CorrelationIDHeader, got, id)
	}
}

func TestInjectCorrelationIDMintsWhenAbsent(t *testing.T) {
	ctx := injectCorrelationID(context.Background())

	id := CorrelationIDFromContext(ctx)
	if id == "" {
		t.Fatal("injectCorrelationID did not mint an ID for a bare context")
	}

	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("expected outgoing metadata to be set")
	}
	if got := md.Get(CorrelationIDHeader); len(got) != 1 || got[0] != id {
		t.Fatalf("outgoing metadata %s = %v, want [%s]", CorrelationIDHeader, got, id)
	}
}
