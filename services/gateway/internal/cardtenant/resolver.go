// Package cardtenant closes the card_id-vs-tenant_id blind spot named in
// docs/THREAT-MODEL.md's "Known gaps" item 1(a) and mirrored in
// internal/ratelimit's keyFor doc comment: GetCard and AdjustCardLimit
// requests carry only a card_id, not a tenant_id, so both
// internal/tenantauth and internal/ratelimit — which every other RPC's
// authorization/rate-limit decision keys off tenant_id — previously let
// these two RPCs through completely unscoped.
//
// Resolver answers "which tenant owns this card?" by asking
// card-issuance (the authoritative source — never trusting whatever
// tenant_id a caller might claim), with a small TTL cache so this
// doesn't turn into an extra synchronous RPC on every single
// GetCard/AdjustCardLimit call: card ownership never changes after
// issuance, so a cached answer is never stale in a way that matters,
// only briefly unavailable for a brand-new card minted less than one
// cache entry's TTL ago (which naturally self-heals on the next lookup
// after the entry expires).
package cardtenant

import (
	"context"
	"fmt"
	"sync"
	"time"

	cardissuancepb "github.com/caelum0x/ration/pkg/pb/cardissuance"
)

// CardGetter is the subset of cardissuanceclient.Client this package
// needs — a narrow interface (rather than depending on the concrete
// *cardissuanceclient.Client type) so tests can supply a fake without
// standing up a real gRPC server to card-issuance. *cardissuanceclient.
// Client's existing GetCard(ctx, cardID string) (*cardissuancepb.Card,
// error) method already satisfies this exactly, with no adapter needed.
type CardGetter interface {
	GetCard(ctx context.Context, cardID string) (*cardissuancepb.Card, error)
}

// Resolver resolves a card_id to the tenant_id that owns it.
type Resolver interface {
	ResolveTenantID(ctx context.Context, cardID string) (string, error)
}

// cacheEntry is one cached (tenantID, expiry) pair.
type cacheEntry struct {
	tenantID string
	expires  time.Time
}

// DefaultTTL bounds how long a resolved card -> tenant mapping is
// trusted before this package asks card-issuance again. Card ownership
// is immutable after issuance, so this only exists to bound memory for
// cards that are no longer being queried (see Cache.sweep) and to let a
// card-issuance-side data correction (e.g. a demo reset) eventually
// propagate, not because the mapping is expected to change.
const DefaultTTL = 10 * time.Minute

// Cache wraps a CardGetter with an in-memory, TTL-bounded cache of
// card_id -> tenant_id. Safe for concurrent use.
type Cache struct {
	getter CardGetter
	ttl    time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry

	now func() time.Time
}

// NewCache constructs a Cache backed by getter, caching resolved
// tenant_ids for ttl (DefaultTTL if ttl <= 0).
func NewCache(getter CardGetter, ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Cache{
		getter:  getter,
		ttl:     ttl,
		entries: make(map[string]cacheEntry),
		now:     time.Now,
	}
}

// ResolveTenantID returns the tenant_id that owns cardID, consulting the
// cache first and falling through to card-issuance (via GetCard) on a
// miss or expired entry. Returns an error if cardID is empty, the card
// doesn't exist, or the lookup itself fails — callers (both
// internal/tenantauth and internal/ratelimit) must treat any error here
// as "cannot establish this request's tenant", not as "unscoped, so let
// it through": the entire point of this package is that an unresolvable
// card_id must never silently bypass tenant-scoped checks.
func (c *Cache) ResolveTenantID(ctx context.Context, cardID string) (string, error) {
	if cardID == "" {
		return "", fmt.Errorf("cardtenant: card_id is required")
	}

	now := c.now()
	c.mu.RLock()
	entry, ok := c.entries[cardID]
	c.mu.RUnlock()
	if ok && now.Before(entry.expires) {
		return entry.tenantID, nil
	}

	card, err := c.getter.GetCard(ctx, cardID)
	if err != nil {
		return "", fmt.Errorf("cardtenant: get card %s: %w", cardID, err)
	}
	tenantID := card.GetTenantId()
	if tenantID == "" {
		return "", fmt.Errorf("cardtenant: card %s has no owning tenant_id", cardID)
	}

	c.mu.Lock()
	c.entries[cardID] = cacheEntry{tenantID: tenantID, expires: now.Add(c.ttl)}
	c.mu.Unlock()

	return tenantID, nil
}

// Sweep deletes every cache entry that expired more than maxIdle ago,
// bounding this cache's memory to roughly the set of cards actually
// queried in the last ttl+maxIdle window rather than every card ever
// looked up. Intended to be called periodically from a background
// goroutine (mirroring internal/ratelimit.Limiter.Sweep's own doc
// comment), never from the request path.
func (c *Cache) Sweep(maxIdle time.Duration) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for cardID, entry := range c.entries {
		if now.Sub(entry.expires) > maxIdle {
			delete(c.entries, cardID)
		}
	}
}
