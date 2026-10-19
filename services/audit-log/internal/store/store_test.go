package store

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestAppend_AssignsSequentialHashChainedRecords(t *testing.T) {
	s, _ := newTestStore(t)

	r1, err := s.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, TenantID: "t1", RefID: "entry-1", PayloadJSON: `{"a":1}`, OccurredAtUnix: 100}, 101)
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if r1.Sequence != 1 {
		t.Fatalf("expected sequence 1, got %d", r1.Sequence)
	}
	if r1.PrevHash != "" {
		t.Fatalf("expected empty prev_hash for genesis record, got %q", r1.PrevHash)
	}
	if r1.Hash == "" {
		t.Fatalf("expected a non-empty hash")
	}

	r2, err := s.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindAgentDecision, TenantID: "t1", RefID: "decision-1", PayloadJSON: `{"b":2}`, OccurredAtUnix: 200}, 201)
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if r2.Sequence != 2 {
		t.Fatalf("expected sequence 2, got %d", r2.Sequence)
	}
	if r2.PrevHash != r1.Hash {
		t.Fatalf("expected record 2's prev_hash to chain to record 1's hash")
	}
}

func TestAppend_IsIdempotentOnSourceKindRefID(t *testing.T) {
	s, _ := newTestStore(t)

	in := AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, TenantID: "t1", RefID: "entry-1", PayloadJSON: `{"a":1}`, OccurredAtUnix: 100}
	r1, err := s.Append(in, 101)
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}

	// Re-append with a different payload/occurred_at (simulating a
	// best-effort caller retrying after an ambiguous failure) — must
	// return the ORIGINAL record, not create a second one or overwrite.
	retryIn := in
	retryIn.PayloadJSON = `{"a":999}`
	retryIn.OccurredAtUnix = 999
	r2, err := s.Append(retryIn, 999)
	if err != nil {
		t.Fatalf("Append (retry): %v", err)
	}
	if r2 != r1 {
		t.Fatalf("expected idempotent re-append to return the original record unchanged:\n got  %+v\n want %+v", r2, r1)
	}

	if got, _ := s.List("", 0, 10); len(got) != 1 {
		t.Fatalf("expected exactly one record after a duplicate append, got %d", len(got))
	}
}

func TestAppend_DifferentKindsAreNotIdempotentTogether(t *testing.T) {
	s, _ := newTestStore(t)
	// Same ref_id but different kind must NOT collide — a LedgerEntry
	// and an AgentDecision can legitimately share an id namespace from
	// different source systems.
	_, err := s.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, RefID: "x", PayloadJSON: "{}"}, 1)
	if err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	_, err = s.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindAgentDecision, RefID: "x", PayloadJSON: "{}"}, 2)
	if err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if got, _ := s.List("", 0, 10); len(got) != 2 {
		t.Fatalf("expected 2 distinct records, got %d", len(got))
	}
}

func TestOpen_RecoversStateFromExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	r1, err := s1.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, TenantID: "t1", RefID: "entry-1", PayloadJSON: "{}"}, 1)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2 (recovery): %v", err)
	}
	defer s2.Close()

	r2, err := s2.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, TenantID: "t1", RefID: "entry-2", PayloadJSON: "{}"}, 2)
	if err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if r2.Sequence != 2 {
		t.Fatalf("expected recovered store to continue sequence at 2, got %d", r2.Sequence)
	}
	if r2.PrevHash != r1.Hash {
		t.Fatalf("expected recovered store's chain to continue from the persisted record's hash")
	}

	result := s2.VerifyChain()
	if !result.Intact {
		t.Fatalf("expected recovered chain to verify intact, got %+v", result)
	}
	if result.RecordsChecked != 2 {
		t.Fatalf("expected 2 records checked, got %d", result.RecordsChecked)
	}
}

func TestOpen_RejectsTamperedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s1.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, RefID: "entry-1", PayloadJSON: "{}"}, 1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := s1.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, RefID: "entry-2", PayloadJSON: "{}"}, 2); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Tamper: rewrite the file with record 1's payload_json changed,
	// without recomputing its hash — simulating exactly the attack this
	// package exists to detect (an edit to an already-appended record).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	tampered := []byte(replaceOnce(string(raw), `"payload_json":"{}"`, `"payload_json":"{\"tampered\":true}"`))
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatalf("write tampered file: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatalf("expected Open to refuse a tampered file, got no error")
	}
}

// replaceOnce replaces the first occurrence of old with new in s.
func replaceOnce(s, old, new string) string {
	idx := indexOf(s, old)
	if idx < 0 {
		return s
	}
	return s[:idx] + new + s[idx+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestList_PaginatesAndFiltersByTenant(t *testing.T) {
	s, _ := newTestStore(t)
	for i := 0; i < 5; i++ {
		tenant := "t1"
		if i%2 == 0 {
			tenant = "t2"
		}
		if _, err := s.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, TenantID: tenant, RefID: "entry-" + string(rune('a'+i)), PayloadJSON: "{}"}, int64(i)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	page1, next1 := s.List("", 0, 2)
	if len(page1) != 2 || next1 == 0 {
		t.Fatalf("expected a full first page with a next token, got %d records, next=%d", len(page1), next1)
	}
	page2, next2 := s.List("", next1, 2)
	if len(page2) != 2 || next2 == 0 {
		t.Fatalf("expected a full second page with a next token, got %d records, next=%d", len(page2), next2)
	}
	page3, next3 := s.List("", next2, 2)
	if len(page3) != 1 || next3 != 0 {
		t.Fatalf("expected a final partial page with no next token, got %d records, next=%d", len(page3), next3)
	}

	t1Records, _ := s.List("t1", 0, 100)
	for _, r := range t1Records {
		if r.TenantID != "t1" {
			t.Fatalf("expected only t1 records, got tenant_id=%q", r.TenantID)
		}
	}
	if len(t1Records) != 2 {
		t.Fatalf("expected 2 t1 records, got %d", len(t1Records))
	}
}

func TestVerifyChain_DetectsHashMismatchWithoutFileTamper(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Append(AppendInput{SourceService: "ledger-core", Kind: SourceKindLedgerEntry, RefID: "entry-1", PayloadJSON: "{}"}, 1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Directly corrupt the in-memory record (simulating a hypothetical
	// future bug that mutates s.records without going through Append)
	// to prove VerifyChain actually recomputes hashes rather than
	// trusting whatever is in memory.
	s.records[0].PayloadJSON = `{"mutated":true}`

	result := s.VerifyChain()
	if result.Intact {
		t.Fatalf("expected VerifyChain to detect the mutated record")
	}
	if result.FirstBrokenSequence != 1 {
		t.Fatalf("expected first_broken_sequence=1, got %d", result.FirstBrokenSequence)
	}
}
