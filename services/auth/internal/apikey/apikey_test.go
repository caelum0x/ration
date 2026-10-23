package apikey

import "testing"

func TestIssueAndVerify_Success(t *testing.T) {
	s := New()
	keyID, secret, createdAt, err := s.Issue("tenant_1", "CLI key", false)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if keyID == "" || secret == "" || createdAt == 0 {
		t.Fatalf("Issue() returned incomplete result: keyID=%q secret-empty=%v createdAt=%d", keyID, secret == "", createdAt)
	}

	result := s.Verify(secret)
	if !result.Valid {
		t.Fatalf("Verify() = %+v, want Valid=true", result)
	}
	if result.KeyID != keyID {
		t.Errorf("KeyID = %q, want %q", result.KeyID, keyID)
	}
	if result.TenantID != "tenant_1" {
		t.Errorf("TenantID = %q, want tenant_1", result.TenantID)
	}
}

func TestIssue_RequiresTenantID(t *testing.T) {
	s := New()
	if _, _, _, err := s.Issue("", "name", false); err == nil {
		t.Fatal("Issue() expected error for empty tenant_id, got nil")
	}
}

func TestIssue_SecretsAreUnique(t *testing.T) {
	s := New()
	_, secret1, _, err := s.Issue("tenant_1", "a", false)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	_, secret2, _, err := s.Issue("tenant_1", "b", false)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if secret1 == secret2 {
		t.Fatal("two Issue() calls produced the same secret")
	}
}

func TestVerify_UnknownSecret(t *testing.T) {
	s := New()
	result := s.Verify("rk_does_not_exist")
	if result.Valid {
		t.Fatal("Verify() = Valid=true for an unknown secret")
	}
	if result.Reason == "" {
		t.Error("Verify() Reason is empty for an invalid result")
	}
}

func TestVerify_EmptySecret(t *testing.T) {
	s := New()
	result := s.Verify("")
	if result.Valid {
		t.Fatal("Verify() = Valid=true for an empty secret")
	}
}

func TestRevoke_InvalidatesKey(t *testing.T) {
	s := New()
	keyID, secret, _, err := s.Issue("tenant_1", "name", false)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}

	if err := s.Revoke(keyID); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	result := s.Verify(secret)
	if result.Valid {
		t.Fatal("Verify() = Valid=true for a revoked key")
	}
	if result.Reason != "revoked" {
		t.Errorf("Reason = %q, want %q", result.Reason, "revoked")
	}
}

func TestRevoke_NotFound(t *testing.T) {
	s := New()
	if err := s.Revoke("key_does_not_exist"); err != ErrNotFound {
		t.Fatalf("Revoke() error = %v, want %v", err, ErrNotFound)
	}
}

func TestRevoke_Idempotent(t *testing.T) {
	s := New()
	keyID, _, _, err := s.Issue("tenant_1", "name", false)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if err := s.Revoke(keyID); err != nil {
		t.Fatalf("first Revoke() error = %v", err)
	}
	if err := s.Revoke(keyID); err != nil {
		t.Fatalf("second Revoke() error = %v, want nil (idempotent)", err)
	}
}
