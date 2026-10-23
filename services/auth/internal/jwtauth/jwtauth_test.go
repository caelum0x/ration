package jwtauth

import (
	"testing"
	"time"
)

func TestMintAndVerify_Success(t *testing.T) {
	s, err := New([]byte("test-signing-key"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	token, expiresAt, err := s.Mint("operator_1", "tenant_1", time.Hour, false)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	if token == "" {
		t.Fatal("Mint() returned empty token")
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("expiresAt = %v, want in the future", expiresAt)
	}

	result := s.Verify(token)
	if !result.Valid {
		t.Fatalf("Verify() = %+v, want Valid=true", result)
	}
	if result.Subject != "operator_1" || result.TenantID != "tenant_1" {
		t.Errorf("Verify() = %+v, unexpected claims", result)
	}
}

func TestNew_RequiresSigningKey(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New() expected error for empty signing key, got nil")
	}
}

func TestMint_RequiresSubject(t *testing.T) {
	s, _ := New([]byte("key"))
	if _, _, err := s.Mint("", "tenant_1", time.Hour, false); err == nil {
		t.Fatal("Mint() expected error for empty subject, got nil")
	}
}

func TestMint_DefaultAndMaxTTL(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	s, _ := New([]byte("key"))
	s.now = func() time.Time { return fixedNow }

	_, expiresAt, err := s.Mint("sub", "tenant_1", 0, false)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	if !expiresAt.Equal(fixedNow.Add(DefaultTTL)) {
		t.Errorf("expiresAt = %v, want %v (DefaultTTL)", expiresAt, fixedNow.Add(DefaultTTL))
	}

	_, expiresAt, err = s.Mint("sub", "tenant_1", 365*24*time.Hour, false)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	if !expiresAt.Equal(fixedNow.Add(MaxTTL)) {
		t.Errorf("expiresAt = %v, want %v (clamped to MaxTTL)", expiresAt, fixedNow.Add(MaxTTL))
	}
}

func TestVerify_WrongSigningKeyFails(t *testing.T) {
	s1, _ := New([]byte("key-one"))
	s2, _ := New([]byte("key-two"))

	token, _, err := s1.Mint("sub", "tenant_1", time.Hour, false)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	result := s2.Verify(token)
	if result.Valid {
		t.Fatal("Verify() = Valid=true for a token signed with a different key")
	}
}

func TestVerify_ExpiredToken(t *testing.T) {
	fixedNow := time.Unix(2_000_000_000, 0)
	s, _ := New([]byte("key"))
	s.now = func() time.Time { return fixedNow }

	token, _, err := s.Mint("sub", "tenant_1", time.Minute, false)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}

	s.now = func() time.Time { return fixedNow.Add(2 * time.Minute) }
	result := s.Verify(token)
	if result.Valid {
		t.Fatal("Verify() = Valid=true for an expired token")
	}
	if result.Reason != ErrExpired.Error() {
		t.Errorf("Reason = %q, want %q", result.Reason, ErrExpired.Error())
	}
}

func TestVerify_MalformedToken(t *testing.T) {
	s, _ := New([]byte("key"))
	for _, tok := range []string{"", "not-a-jwt", "a.b", "a.b.c.d", "a..c"} {
		result := s.Verify(tok)
		if result.Valid {
			t.Errorf("Verify(%q) = Valid=true, want false", tok)
		}
	}
}

func TestVerify_TamperedPayload(t *testing.T) {
	s, _ := New([]byte("key"))
	token, _, err := s.Mint("sub", "tenant_1", time.Hour, false)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	tampered := token[:len(token)-4] + "AAAA"
	result := s.Verify(tampered)
	if result.Valid {
		t.Fatal("Verify() = Valid=true for a tampered token")
	}
}
