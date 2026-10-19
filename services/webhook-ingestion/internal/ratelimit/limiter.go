// Package ratelimit implements a per-key token-bucket rate limiter and an
// HTTP middleware that applies it to webhook-ingestion's plain
// net/http webhook listener (POST /webhooks/airwallex).
//
// This is webhook-ingestion's own copy of gateway's
// internal/ratelimit (docs/adr/0004: each split service owns its own
// copy of shared-shape internal utilities, matching ledgerclient/idgen)
// — the token-bucket mechanics are the same, but the integration point
// differs: gateway fronts gRPC unary/stream RPCs keyed by tenant_id,
// while webhook-ingestion fronts a single plain-HTTP route that has no
// notion of a tenant until after the body is parsed, so this package
// keys by source IP instead (docs/THREAT-MODEL.md's "Denial of
// service" row's precise TODO). It exists so a flood of (even
// validly-signed, replayed) webhook POSTs from one source can't starve
// legitimate Airwallex deliveries before they even reach signature
// verification.
package ratelimit

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Limiter is a token bucket per key (here, always "ip:<addr>" — see
// clientIP). Safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	// RatePerSecond is how many tokens refill per second.
	// BurstSize is the bucket's capacity (and its size when first seen).
	RatePerSecond float64
	BurstSize     float64

	// now is overridable in tests; defaults to time.Now.
	now func() time.Time
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastUsed   time.Time
}

// New constructs a Limiter refilling ratePerSecond tokens/sec per key, up
// to a maximum burst of burstSize tokens. Both must be positive.
func New(ratePerSecond, burstSize float64) *Limiter {
	if ratePerSecond <= 0 {
		ratePerSecond = 1
	}
	if burstSize <= 0 {
		burstSize = 1
	}
	return &Limiter{
		buckets:       make(map[string]*bucket),
		RatePerSecond: ratePerSecond,
		BurstSize:     burstSize,
		now:           time.Now,
	}
}

// Allow reports whether one token is available for key right now,
// consuming it if so. Each previously-unseen key starts with a full
// bucket (BurstSize tokens) so a source's very first requests aren't
// throttled before the rate limiter has observed any history for it.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.BurstSize, lastRefill: now}
		l.buckets[key] = b
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.RatePerSecond
		if b.tokens > l.BurstSize {
			b.tokens = l.BurstSize
		}
		b.lastRefill = now
	}
	b.lastUsed = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Sweep deletes any bucket not used in the last maxIdle, bounding the
// map's memory to roughly the set of source IPs active in the last
// maxIdle window rather than every IP ever seen. Intended to be called
// periodically from a background goroutine (see
// services/webhook-ingestion/cmd/webhook-ingestion/main.go), never from
// the request path.
func (l *Limiter) Sweep(maxIdle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key, b := range l.buckets {
		if now.Sub(b.lastUsed) > maxIdle {
			delete(l.buckets, key)
		}
	}
}

// clientIP derives the rate-limit key for an incoming request: the
// request's source address, with the port stripped where present.
// Deliberately does NOT trust X-Forwarded-For/X-Real-IP — those are
// spoofable by anyone who can reach this listener directly, which would
// let an attacker pick any key they like and dodge limiting entirely; if
// this service is ever deployed behind a proxy that must be trusted for
// the real client IP, that proxy should be the one enforcing this limit.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		host = "unknown"
	}
	return "ip:" + host
}

// Middleware wraps next with l's rate limiting, rejecting requests whose
// source IP has exhausted its bucket with 429 Too Many Requests before
// next (and therefore signature verification) ever runs.
func Middleware(l *Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r)) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
