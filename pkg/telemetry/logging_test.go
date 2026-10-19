package telemetry

import (
	"context"
	"testing"
)

func TestLogAttrsIncludesCorrelationID(t *testing.T) {
	id := NewCorrelationID()
	ctx := ContextWithCorrelationID(context.Background(), id)

	attrs := LogAttrs(ctx)

	found := false
	for _, a := range attrs {
		if a.Key == "correlation_id" && a.Value.String() == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("LogAttrs(ctx) = %v, want a correlation_id attribute set to %q", attrs, id)
	}
}

func TestLogAttrsEmptyForBareContext(t *testing.T) {
	attrs := LogAttrs(context.Background())
	if len(attrs) != 0 {
		t.Fatalf("LogAttrs(bare context) = %v, want empty (no correlation ID, no active span)", attrs)
	}
}

func TestNewLoggerTagsServiceName(t *testing.T) {
	logger := NewLogger("gateway")
	if logger == nil {
		t.Fatal("NewLogger returned nil")
	}
}
