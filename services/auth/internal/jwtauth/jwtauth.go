// Package jwtauth mints and verifies short-lived HS256 JWTs for the
// TUI/CLI (auth.proto's MintJWT/VerifyJWT) — new capability introduced
// by the services/auth split (docs/SCALE-EXPANSION.md section 1,
// docs/adr/0004).
//
// This is a minimal, dependency-free HS256 JWT implementation (header.
// payload.signature, base64url, HMAC-SHA256) rather than a pulled-in
// library: the claim set this project needs (subject, tenant_id, exp,
// iat) is small and fixed, and HS256 is the right algorithm for a single
// service that both mints and verifies its own tokens (no asymmetric key
// distribution problem to solve, unlike IssueServiceCert's CA).
package jwtauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// DefaultTTL is used when a caller requests ttl_seconds=0.
const DefaultTTL = 15 * time.Minute

// MaxTTL bounds how long-lived a minted token may be, regardless of what
// a caller requests — a bearer token with no practical expiry defeats
// the point of short-lived tokens.
const MaxTTL = 24 * time.Hour

var (
	// ErrMalformed is returned by Verify for a token that isn't
	// well-formed JWT (wrong number of segments, bad base64, bad JSON).
	ErrMalformed = errors.New("jwtauth: malformed token")
	// ErrBadSignature is returned by Verify when the signature doesn't
	// match what signingKey would have produced.
	ErrBadSignature = errors.New("jwtauth: signature verification failed")
	// ErrExpired is returned by Verify for a token past its exp claim.
	ErrExpired = errors.New("jwtauth: token expired")
)

var header = map[string]string{"alg": "HS256", "typ": "JWT"}

type claims struct {
	Subject  string `json:"sub"`
	TenantID string `json:"tenant_id"`
	// IsOperator marks a cross-tenant token (see proto/auth.proto's
	// MintJWTRequest.is_operator doc comment). TenantID is always "" for
	// an operator token.
	IsOperator bool  `json:"is_operator,omitempty"`
	IssuedAt   int64 `json:"iat"`
	Expires    int64 `json:"exp"`
}

// Signer mints and verifies tokens under one HMAC signing key.
type Signer struct {
	key []byte
	now func() time.Time
}

// New constructs a Signer. signingKey must be non-empty — callers should
// source it from an operator-provided secret (AUTH_JWT_SIGNING_KEY), not
// a hardcoded default, since anyone holding it can mint valid tokens for
// any subject/tenant.
func New(signingKey []byte) (*Signer, error) {
	if len(signingKey) == 0 {
		return nil, fmt.Errorf("jwtauth: signing key must not be empty")
	}
	return &Signer{key: signingKey, now: time.Now}, nil
}

func b64(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func (s *Signer) sign(signingInput string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(signingInput))
	return b64(mac.Sum(nil))
}

// Mint produces a signed token for (subject, tenantID), expiring after
// ttl (clamped to (0, MaxTTL], defaulting to DefaultTTL when ttl <= 0).
//
// isOperator mints a cross-tenant token instead (see
// proto/auth.proto's MintJWTRequest.is_operator doc comment); when true,
// tenantID must be empty.
func (s *Signer) Mint(subject, tenantID string, ttl time.Duration, isOperator bool) (token string, expiresAt time.Time, err error) {
	if subject == "" {
		return "", time.Time{}, fmt.Errorf("jwtauth: subject is required")
	}
	if isOperator && tenantID != "" {
		return "", time.Time{}, fmt.Errorf("jwtauth: tenant_id must be empty for an operator token")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}

	now := s.now()
	exp := now.Add(ttl)

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("jwtauth: encode header: %w", err)
	}
	payloadJSON, err := json.Marshal(claims{
		Subject:    subject,
		TenantID:   tenantID,
		IsOperator: isOperator,
		IssuedAt:   now.Unix(),
		Expires:    exp.Unix(),
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("jwtauth: encode claims: %w", err)
	}

	signingInput := b64(headerJSON) + "." + b64(payloadJSON)
	signature := s.sign(signingInput)
	return signingInput + "." + signature, exp, nil
}

// Result is the outcome of verifying a token.
type Result struct {
	Valid      bool
	Subject    string
	TenantID   string
	IsOperator bool
	Reason     string // explains an invalid result; empty when Valid.
}

// Verify checks token's signature and expiry.
func (s *Signer) Verify(token string) Result {
	parts := splitJWT(token)
	if parts == nil {
		return Result{Reason: ErrMalformed.Error()}
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]

	expectedSig := s.sign(headerB64 + "." + payloadB64)
	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(sigB64)) != 1 {
		return Result{Reason: ErrBadSignature.Error()}
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return Result{Reason: ErrMalformed.Error()}
	}
	var c claims
	if err := json.Unmarshal(payloadJSON, &c); err != nil {
		return Result{Reason: ErrMalformed.Error()}
	}

	if s.now().Unix() > c.Expires {
		return Result{Subject: c.Subject, TenantID: c.TenantID, IsOperator: c.IsOperator, Reason: ErrExpired.Error()}
	}

	return Result{Valid: true, Subject: c.Subject, TenantID: c.TenantID, IsOperator: c.IsOperator}
}

// splitJWT returns the three dot-separated segments of token, or nil if
// token is not shaped like a JWT.
func splitJWT(token string) []string {
	parts := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	if len(parts) != 3 {
		return nil
	}
	for _, p := range parts {
		if p == "" {
			return nil
		}
	}
	return parts
}
