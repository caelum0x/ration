package eventbus

import (
	"testing"
	"time"
)

func TestBus_PublishDeliversToMatchingSubscriber(t *testing.T) {
	b := NewBus()
	ch, unsubscribe := b.Subscribe("tenant_1")
	defer unsubscribe()

	b.Publish(Event{ID: "e1", TenantID: "tenant_1", Kind: "card", Summary: "issued"})
	b.Publish(Event{ID: "e2", TenantID: "tenant_2", Kind: "card", Summary: "other tenant"})

	select {
	case got := <-ch:
		if got.ID != "e1" {
			t.Errorf("ID = %q, want e1", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for matching event")
	}

	select {
	case got := <-ch:
		t.Fatalf("received unexpected second event %+v (should have been filtered out)", got)
	case <-time.After(50 * time.Millisecond):
		// expected: no more events for this tenant filter.
	}
}

func TestBus_WildcardSubscriberReceivesEverything(t *testing.T) {
	b := NewBus()
	ch, unsubscribe := b.Subscribe("")
	defer unsubscribe()

	b.Publish(Event{ID: "e1", TenantID: "tenant_1"})
	b.Publish(Event{ID: "e2", TenantID: "tenant_2"})

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case e := <-ch:
			got[e.ID] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for events")
		}
	}
	if !got["e1"] || !got["e2"] {
		t.Errorf("got = %v, want both e1 and e2", got)
	}
}

func TestBus_UnsubscribeStopsDelivery(t *testing.T) {
	b := NewBus()
	ch, unsubscribe := b.Subscribe("")
	unsubscribe()

	b.Publish(Event{ID: "e1", TenantID: "tenant_1"})

	if _, open := <-ch; open {
		t.Error("channel should be closed after unsubscribe")
	}
	if b.SubscriberCount() != 0 {
		t.Errorf("SubscriberCount() = %d, want 0", b.SubscriberCount())
	}
}

func TestBus_SlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	b := NewBus()
	_, unsubscribe := b.Subscribe("") // never drained
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBufferSize*2; i++ {
			b.Publish(Event{ID: "e", TenantID: "t"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow/undrained subscriber")
	}
}

func TestNewEvent(t *testing.T) {
	e := NewEvent("tenant_1", "card", "issued card")
	if e.ID == "" {
		t.Error("NewEvent() ID is empty")
	}
	if e.TenantID != "tenant_1" || e.Kind != "card" || e.Summary != "issued card" {
		t.Errorf("NewEvent() = %+v, unexpected fields", e)
	}
	if e.AtUnix == 0 {
		t.Error("NewEvent() AtUnix is zero")
	}
}
