// Package ratelimit implements a per-key token-bucket rate limiter and
// the gRPC interceptors that apply it to gateway's public-facing
// surface (gateway.proto's Gateway service — the one boundary the
// TUI/CLI/agent, and ultimately whoever is driving them, actually
// cross). It exists so one noisy or compromised caller can't starve
// every other tenant's requests at the only service external clients
// talk to directly.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"github.com/caelum0x/ration/services/gateway/internal/cardtenant"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Limiter is a token bucket per key (normally "tenant:<id>", falling
// back to "peer:<addr>" for requests that don't carry a tenant_id — see
// keyFor). Safe for concurrent use.
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

// New constructs a Limiter refilling ratePerSecond tokens/sec per key,
// up to a maximum burst of burstSize tokens. Both must be positive.
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
// bucket (BurstSize tokens) so a tenant's very first requests aren't
// throttled before the rate limiter has observed any history for them.
func (l *Limiter) Allow(key string) bool {
	return l.AllowN(key, 1)
}

// AllowN is Allow for n tokens at once (e.g. weighting an expensive RPC
// higher than a cheap one). Returns false, consuming nothing, if fewer
// than n tokens are currently available.
func (l *Limiter) AllowN(key string, n float64) bool {
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

	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// Sweep deletes any bucket not used in the last maxIdle, bounding the
// map's memory to roughly the set of tenants active in the last
// maxIdle window rather than every tenant ever seen. Intended to be
// called periodically from a background goroutine (see
// services/gateway/cmd/gateway/main.go), never from the request path.
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

// tenantIDFieldName is the proto field name every gateway.proto request
// that is scoped to a single tenant uses (see proto/gateway.proto).
// Extracted generically via reflection rather than a type switch per
// RPC so a newly added request message is automatically rate-limited
// per-tenant the moment it adds a tenant_id field, with zero changes
// here.
const tenantIDFieldName = "tenant_id"

// cardIDFieldName is the proto field name GetCard/AdjustCardLimit carry
// instead of tenant_id — see keyFor's doc comment.
const cardIDFieldName = "card_id"

// keyFor derives the bucket key for a unary request: "tenant:<id>" if
// the request message has a non-empty tenant_id field (every
// tenant-scoped RPC in gateway.proto does); failing that, if it carries
// a card_id (GetCard/AdjustCardLimit) and cardResolver is non-nil, the
// resolved owning tenant's "tenant:<id>" key — so a card-issuance outage
// or an unresolvable card_id still falls back to "peer:<addr>" rather
// than failing the request outright (rate-limit *degradation*, unlike
// internal/tenantauth's authorization check, is allowed to be lossy:
// worst case a noisy caller is rate-limited per-connection instead of
// per-tenant, not that it bypasses limiting altogether).
//
// Before cardResolver existed, GetCard/AdjustCardLimit were *always*
// rate-limited per *caller address* rather than per tenant that owns the
// card, because resolving card_id -> tenant_id would have meant an extra
// RPC on every call just to decide whether to allow it. internal/
// cardtenant's cache keeps that cost low enough that paying it buys real
// per-tenant limiting instead.
func keyFor(ctx context.Context, cardResolver cardtenant.Resolver, req any) string {
	if m, ok := req.(proto.Message); ok {
		fd := m.ProtoReflect().Descriptor().Fields().ByName(tenantIDFieldName)
		if fd != nil {
			if tenantID := m.ProtoReflect().Get(fd).String(); tenantID != "" {
				return "tenant:" + tenantID
			}
		}
		if cardResolver != nil {
			if cardFD := m.ProtoReflect().Descriptor().Fields().ByName(cardIDFieldName); cardFD != nil {
				if cardID := m.ProtoReflect().Get(cardFD).String(); cardID != "" {
					if tenantID, err := cardResolver.ResolveTenantID(ctx, cardID); err == nil && tenantID != "" {
						return "tenant:" + tenantID
					}
				}
			}
		}
	}
	return "peer:" + peerAddr(ctx)
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}

// UnaryServerInterceptor rejects a unary RPC with codes.ResourceExhausted
// once its key's bucket (see keyFor) is empty. cardResolver may be nil
// (falls back to the pre-existing peer-address keying for GetCard/
// AdjustCardLimit — see keyFor's doc comment).
func UnaryServerInterceptor(l *Limiter, cardResolver cardtenant.Resolver) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		key := keyFor(ctx, cardResolver, req)
		if !l.Allow(key) {
			return nil, status.Errorf(codes.ResourceExhausted, "ratelimit: rate limit exceeded for %s", info.FullMethod)
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor is UnaryServerInterceptor for streaming RPCs
// (gateway.proto's StreamActivity). Streaming requests are keyed by
// peer address rather than tenant_id: StreamActivityRequest.tenant_id
// is allowed to be empty (meaning "all tenants"), so keying by tenant
// here would let an all-tenants stream dodge limiting by using a fresh
// empty-tenant request each time; a long-lived stream is also a single
// connection resource regardless of which tenant it's scoped to, so
// per-peer accounting is the more honest model.
func StreamServerInterceptor(l *Limiter) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		key := "peer:" + peerAddr(ss.Context())
		if !l.Allow(key) {
			return status.Errorf(codes.ResourceExhausted, "ratelimit: rate limit exceeded for %s", info.FullMethod)
		}
		return handler(srv, ss)
	}
}
