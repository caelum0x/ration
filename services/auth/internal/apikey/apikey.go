// Package apikey implements tenant API-key issuance, verification and
// revocation (auth.proto's IssueAPIKey/VerifyAPIKey/RevokeAPIKey) — new
// capability introduced by the services/auth split
// (docs/SCALE-EXPANSION.md section 1, docs/adr/0004), not an extraction
// of existing gateway behavior: nothing in this project previously
// issued or verified API keys at all.
//
// Secrets are high-entropy (16 random bytes, ~128 bits) tokens, so —
// unlike user-chosen passwords, which need a slow, salted KDF (bcrypt/
// argon2) to resist offline guessing — a direct SHA-256 hash is the
// correct, standard practice here (the same approach GitHub and Stripe
// use for their own API tokens): the hash itself doubles as the lookup
// index, and brute-forcing a hash back to a 128-bit random secret is
// computationally infeasible regardless of hash speed.
//
// Storage is in-memory, matching the demo-scale pattern used throughout
// this codebase (e.g. services/card-issuance/internal/cardsvc.Service's
// in-memory card map): every issued key and its revocation state is lost
// on restart. TODO(production): persist key records (key_id, tenant_id,
// secret_hash, name, created_at, revoked) in a real datastore — the
// obvious target is a new ledger-core-adjacent table or a dedicated
// Postgres instance, not ledger-core itself (API keys are not money
// state) — before running this service anywhere issued keys need to
// outlive a process restart.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNotFound is returned by Revoke when key_id does not correspond to
// any issued key.
var ErrNotFound = errors.New("apikey: key not found")

type record struct {
	keyID         string
	tenantID      string
	name          string
	secretHashHex string
	createdAtUnix int64
	revoked       bool
	// isOperator marks a cross-tenant credential (see
	// proto/auth.proto's IssueAPIKeyRequest.is_operator doc comment).
	// tenantID is always "" for an operator record.
	isOperator bool
}

// Store issues, verifies and revokes API keys. Safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	byID map[string]*record
	// byHash indexes the same records by secret hash, since VerifyAPIKey
	// is only ever given the presented secret, never the key_id.
	byHash map[string]*record

	now   func() time.Time
	newID func(prefix string) string
}

// New constructs an empty Store.
func New() *Store {
	return &Store{
		byID:   make(map[string]*record),
		byHash: make(map[string]*record),
		now:    time.Now,
		newID:  newRandomID,
	}
}

func newRandomID(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("apikey: system random source unavailable: %v", err))
	}
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(buf[:]))
}

// newSecret generates a fresh high-entropy API key secret: 24 random
// bytes (192 bits) hex-encoded, prefixed so a leaked secret is
// recognizable as a Ration API key in logs/scanners.
func newSecret() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("apikey: system random source unavailable: %w", err)
	}
	return "rk_" + hex.EncodeToString(buf[:]), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Issue mints a new API key for tenantID, returning the key's ID and its
// plaintext secret. The secret is never stored or retrievable again
// after this call returns — only its hash is kept.
//
// isOperator mints a cross-tenant credential instead (see
// proto/auth.proto's IssueAPIKeyRequest.is_operator doc comment); when
// true, tenantID must be empty — an operator key is not bound to any one
// tenant, so supplying one would be misleading about what the key can
// actually do.
func (s *Store) Issue(tenantID, name string, isOperator bool) (keyID, secret string, createdAtUnix int64, err error) {
	if isOperator {
		if tenantID != "" {
			return "", "", 0, fmt.Errorf("apikey: tenant_id must be empty for an operator key")
		}
	} else if tenantID == "" {
		return "", "", 0, fmt.Errorf("apikey: tenant_id is required")
	}
	secret, err = newSecret()
	if err != nil {
		return "", "", 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	keyID = s.newID("key")
	createdAtUnix = s.now().Unix()
	rec := &record{
		keyID:         keyID,
		tenantID:      tenantID,
		name:          name,
		secretHashHex: hashSecret(secret),
		createdAtUnix: createdAtUnix,
		isOperator:    isOperator,
	}
	s.byID[keyID] = rec
	s.byHash[rec.secretHashHex] = rec
	return keyID, secret, createdAtUnix, nil
}

// VerifyResult is the outcome of verifying a presented secret.
type VerifyResult struct {
	Valid      bool
	KeyID      string
	TenantID   string
	IsOperator bool
	Reason     string // explains an invalid result; empty when Valid.
}

// Verify checks a presented secret against every issued, non-revoked
// key's hash.
func (s *Store) Verify(secret string) VerifyResult {
	if secret == "" {
		return VerifyResult{Reason: "empty secret"}
	}
	hash := hashSecret(secret)

	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.byHash[hash]
	if !ok {
		return VerifyResult{Reason: "not found"}
	}
	if rec.revoked {
		return VerifyResult{KeyID: rec.keyID, TenantID: rec.tenantID, IsOperator: rec.isOperator, Reason: "revoked"}
	}
	return VerifyResult{Valid: true, KeyID: rec.keyID, TenantID: rec.tenantID, IsOperator: rec.isOperator}
}

// Revoke immediately invalidates keyID. Returns ErrNotFound if keyID was
// never issued. Idempotent: revoking an already-revoked key succeeds.
func (s *Store) Revoke(keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.byID[keyID]
	if !ok {
		return ErrNotFound
	}
	rec.revoked = true
	return nil
}
