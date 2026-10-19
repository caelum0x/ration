package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
		if !l.Allow("ip:1.2.3.4") {
			t.Fatalf("expected request %d within burst to be allowed", i)
		}
	}
	if l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected request beyond burst to be denied")
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	l, clock := newTestLimiter(2 /* tokens/sec */, 2)
	if !l.Allow("ip:1.2.3.4") || !l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected initial burst of 2 to be allowed")
	}
	if l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected bucket to be empty")
	}

	clock.advance(500 * time.Millisecond) // refills 1 token at 2/sec
	if !l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected 1 token to have refilled after 500ms at 2/sec")
	}
	if l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected bucket to be empty again immediately after")
	}
}

func TestAllow_RefillCappedAtBurst(t *testing.T) {
	l, clock := newTestLimiter(100, 2)
	l.Allow("ip:1.2.3.4") // tokens: 1 remaining
	clock.advance(10 * time.Second)
	// Even though 100*10=1000 tokens worth of time passed, the bucket
	// caps at BurstSize.
	if !l.Allow("ip:1.2.3.4") || !l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected refill to cap at burst size (2), not grow unbounded")
	}
	if l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected exactly burst-size tokens available after a long idle period")
	}
}

func TestAllow_KeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	if !l.Allow("ip:1.2.3.4") {
		t.Fatalf("expected first source's first request to be allowed")
	}
	if !l.Allow("ip:5.6.7.8") {
		t.Fatalf("expected second source to have its own independent bucket")
	}
}

func TestSweep_EvictsOnlyIdleBuckets(t *testing.T) {
	l, clock := newTestLimiter(1, 1)
	l.Allow("ip:idle")
	clock.advance(time.Hour)
	l.Allow("ip:active")

	l.Sweep(30 * time.Minute)

	l.mu.Lock()
	_, idleStillPresent := l.buckets["ip:idle"]
	_, activeStillPresent := l.buckets["ip:active"]
	l.mu.Unlock()

	if idleStillPresent {
		t.Fatalf("expected idle bucket to be swept")
	}
	if !activeStillPresent {
		t.Fatalf("expected recently-used bucket to survive the sweep")
	}
}

func TestClientIP_StripsPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", nil)
	r.RemoteAddr = "203.0.113.9:54321"
	if got := clientIP(r); got != "ip:203.0.113.9" {
		t.Fatalf("clientIP() = %q, want %q", got, "ip:203.0.113.9")
	}
}

func TestClientIP_FallsBackToRawRemoteAddrWithoutPort(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", nil)
	r.RemoteAddr = "not-a-host-port"
	if got := clientIP(r); got != "ip:not-a-host-port" {
		t.Fatalf("clientIP() = %q, want %q", got, "ip:not-a-host-port")
	}
}

func TestMiddleware_AllowsThenDenies(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	handlerCalls := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls++
		w.WriteHeader(http.StatusOK)
	})
	mw := Middleware(l, next)

	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", nil)
		r.RemoteAddr = "203.0.113.9:54321"
		return r
	}

	rec1 := httptest.NewRecorder()
	mw.ServeHTTP(rec1, req())
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	mw.ServeHTTP(rec2, req())
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec2.Code)
	}
	if handlerCalls != 1 {
		t.Fatalf("expected next handler invoked exactly once, got %d", handlerCalls)
	}
}

func TestMiddleware_IndependentSourceIPs(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mw := Middleware(l, next)

	reqA := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", nil)
	reqA.RemoteAddr = "203.0.113.9:54321"
	recA := httptest.NewRecorder()
	mw.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("source A: status = %d, want 200", recA.Code)
	}

	reqB := httptest.NewRequest(http.MethodPost, "/webhooks/airwallex", nil)
	reqB.RemoteAddr = "198.51.100.4:12345"
	recB := httptest.NewRecorder()
	mw.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("expected source B's own bucket to be unaffected by source A's usage, got status %d", recB.Code)
	}
}
