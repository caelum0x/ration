// Package eventbus implements a small Go-channel fan-out event bus that
// feeds webhook_ingestion.proto's StreamEvents RPC. Every normalized
// webhook event (e.g. a deposit received) publishes an Event here;
// services/gateway (the one documented subscriber, per
// webhook_ingestion.proto's doc comment) merges this stream into its own
// activity feed for the TUI.
//
// docs/SCALE-EXPANSION.md section 1 is explicit that this in-process bus
// is a placeholder: "a simple internal event bus for now — document
// where a real deployment would swap in SQS/Kafka." A real deployment
// running more than one webhook-ingestion replica needs a shared broker
// (SQS FIFO keyed by tenant_id, or a Kafka topic partitioned the same
// way) so every subscriber sees every event exactly once regardless of
// which replica received the originating webhook — this bus only fans
// out to subscribers of *this* process, which is fine for the
// single-replica deployment this project ships today but is the first
// thing to swap out before running N > 1 replicas for real.
package eventbus

import (
	"sync"
	"time"

	"github.com/caelum0x/ration/services/webhook-ingestion/internal/idgen"
)

// Event mirrors webhook_ingestion.proto's NormalizedEvent, kept as a
// plain Go struct here so this package has zero dependency on generated
// pb types or gRPC — internal/grpcserver does that mapping at the edge.
type Event struct {
	ID       string
	TenantID string
	Kind     string // "deposit", ...
	Summary  string
	AtUnix   int64
}

// NewEvent fills in ID and AtUnix for a caller that only has the
// business fields to report.
func NewEvent(tenantID, kind, summary string) Event {
	return Event{
		ID:       idgen.New("evt"),
		TenantID: tenantID,
		Kind:     kind,
		Summary:  summary,
		AtUnix:   time.Now().Unix(),
	}
}

// subscriberBufferSize bounds how many unread events a slow subscriber
// can accumulate before Publish starts dropping its oldest unread event
// rather than blocking every other subscriber (and the publisher) on one
// stuck consumer — appropriate for a live event feed where a dropped
// stale event is better than a wedged gRPC stream.
const subscriberBufferSize = 256

// Bus is a fan-out event bus: every Publish is delivered to every
// subscriber whose tenant filter matches (empty filter = all tenants).
type Bus struct {
	mu     sync.Mutex
	nextID int64
	subs   map[int64]*subscriber
}

type subscriber struct {
	tenantFilter string // "" = all tenants
	ch           chan Event
}

// NewBus constructs an empty event bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int64]*subscriber)}
}

// Subscribe registers a new subscriber scoped to tenantFilter ("" for
// every tenant) and returns a receive-only channel plus an unsubscribe
// function the caller must call exactly once when done (e.g. via
// defer) to release resources.
func (b *Bus) Subscribe(tenantFilter string) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextID
	b.nextID++
	sub := &subscriber{
		tenantFilter: tenantFilter,
		ch:           make(chan Event, subscriberBufferSize),
	}
	b.subs[id] = sub

	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok {
			close(existing.ch)
			delete(b.subs, id)
		}
	}
	return sub.ch, unsubscribe
}

// Publish fans e out to every matching subscriber. Delivery is
// non-blocking per subscriber: if a subscriber's buffer is full, the
// oldest buffered event for that subscriber is dropped to make room,
// rather than blocking the publisher (and every other subscriber) on one
// slow consumer.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, sub := range b.subs {
		if sub.tenantFilter != "" && sub.tenantFilter != e.TenantID {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			// Buffer full: drop the oldest event, then push the new one.
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- e:
			default:
				// Extremely unlikely race with another drain; skip rather
				// than block.
			}
		}
	}
}

// SubscriberCount reports the current number of active subscribers,
// primarily for tests and health/debug introspection.
func (b *Bus) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
